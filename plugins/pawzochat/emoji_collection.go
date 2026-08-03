package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
)

const (
	emojiCollectionWorkers  = 2
	emojiCollectionQueue    = 32
	maxCollectedEmojiBytes  = 8 * 1024 * 1024
	maxCollectedEmojiPixels = 40_000_000
	emojiMediaHTTPTimeout   = 5 * time.Second
	emojiMediaCDNTimeout    = 8 * time.Second
)

type emojiCollectionJob struct {
	Config      Config
	PersonaID   string
	SessionKey  string
	SessionName string
	SenderID    string
	SenderName  string
	WeChatMD5   string
	FileID      string
	CDNURL      string
	AESKey      string
	Data        []byte
}

type emojiCandidateResponse struct {
	Outcome string `json:"outcome"`
	Error   string `json:"error"`
}

func buildEmojiCollectionJob(
	msg *message.Message,
	self *contact.SelfInfo,
	config Config,
) (emojiCollectionJob, bool) {
	if msg == nil || msg.GetEmoji() == nil || msg.GetEmoji().GetMedia() == nil {
		return emojiCollectionJob{}, false
	}
	sender := msg.GetSender()
	if sender == nil || strings.TrimSpace(sender.GetUsername()) == "" {
		return emojiCollectionJob{}, false
	}
	media := msg.GetEmoji().GetMedia()
	md5 := strings.ToLower(strings.TrimSpace(media.GetMd5()))
	if !validMD5(md5) {
		return emojiCollectionJob{}, false
	}

	isChatroom := sender.GetType() == contactTypeChatroom
	sessionKey := "private:" + sender.GetUsername()
	sessionName := displayContact(sender)
	senderID := sender.GetUsername()
	senderName := displayContact(sender)
	if isChatroom {
		sessionKey = "chatroom:" + sender.GetUsername()
		sessionName = displayContact(sender)
		senderID = strings.TrimSpace(msg.GetMember().GetUsername())
		senderName = displayMember(msg.GetMember())
	}
	if senderID != "" && senderID == strings.TrimSpace(self.GetUsername()) {
		return emojiCollectionJob{}, false
	}
	personaID := strings.TrimSpace(config.Routes[sessionKey])
	if personaID == "" {
		personaID = strings.TrimSpace(config.DefaultPersonaID)
	}
	if personaID == "" {
		return emojiCollectionJob{}, false
	}
	return emojiCollectionJob{
		Config: config, PersonaID: personaID, SessionKey: sessionKey,
		SessionName: sessionName, SenderID: senderID, SenderName: senderName,
		WeChatMD5: md5, FileID: md5, CDNURL: strings.TrimSpace(media.GetUrl()),
		AESKey: strings.TrimSpace(media.GetKey()), Data: append([]byte(nil), media.GetData()...),
	}, true
}

func validMD5(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func (p *PawzoChatPlugin) startEmojiWorkers() {
	p.emojiMu.Lock()
	defer p.emojiMu.Unlock()
	if p.emojiQueue != nil {
		return
	}
	p.emojiQueue = make(chan emojiCollectionJob, emojiCollectionQueue)
	p.emojiStop = make(chan struct{})
	queue, stop := p.emojiQueue, p.emojiStop
	for range emojiCollectionWorkers {
		p.emojiWG.Add(1)
		go func() {
			defer p.emojiWG.Done()
			for {
				select {
				case <-stop:
					return
				case job := <-queue:
					p.processEmojiCollection(job)
				}
			}
		}()
	}
}

func (p *PawzoChatPlugin) stopEmojiWorkers() {
	p.emojiMu.Lock()
	if p.emojiQueue == nil {
		p.emojiMu.Unlock()
		return
	}
	close(p.emojiStop)
	p.emojiQueue = nil
	p.emojiStop = nil
	p.emojiMu.Unlock()
	p.emojiWG.Wait()
}

func (p *PawzoChatPlugin) enqueueEmojiCollection(job emojiCollectionJob) {
	p.emojiMu.Lock()
	queue, stop := p.emojiQueue, p.emojiStop
	p.emojiMu.Unlock()
	if queue == nil {
		return
	}
	select {
	case <-stop:
		return
	case queue <- job:
	default:
		slog.Warn("[pawzochat] 表情包收集队列已满，已跳过候选")
	}
}

func (p *PawzoChatPlugin) processEmojiCollection(job emojiCollectionJob) {
	response, err := p.checkEmojiCandidate(job)
	if err != nil {
		slog.Warn("[pawzochat] 检查表情包候选失败", "err", err)
		return
	}
	if response.Outcome != "upload_required" {
		return
	}
	raw, contentType, extension, err := p.downloadEmoji(job)
	if err != nil {
		slog.Warn("[pawzochat] 下载表情包候选失败", "wechat_md5", job.WeChatMD5, "err", err)
		return
	}
	if err := p.uploadEmojiCandidate(job, raw, contentType, extension); err != nil {
		slog.Warn("[pawzochat] 上传表情包候选失败", "err", err)
	}
}

func (p *PawzoChatPlugin) checkEmojiCandidate(job emojiCollectionJob) (emojiCandidateResponse, error) {
	payload, err := json.Marshal(emojiCandidateFields(job))
	if err != nil {
		return emojiCandidateResponse{}, err
	}
	endpoint := strings.TrimRight(job.Config.BaseURL, "/") + "/api/bridge/golem/emoji-candidates/check"
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return emojiCandidateResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	setBridgeAuthorization(request, job.Config.Token)
	return doEmojiCandidateRequest(request, emojiHTTPTimeout(job.Config))
}

func (p *PawzoChatPlugin) uploadEmojiCandidate(
	job emojiCollectionJob,
	raw []byte,
	contentType string,
	extension string,
) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range emojiCandidateFields(job) {
		if err := writer.WriteField(key, value); err != nil {
			return err
		}
	}
	part, err := writer.CreateFormFile("emoji", "emoji"+extension)
	if err != nil {
		return err
	}
	if _, err := part.Write(raw); err != nil {
		return err
	}
	if err := writer.WriteField("detected_mime_type", contentType); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	endpoint := strings.TrimRight(job.Config.BaseURL, "/") + "/api/bridge/golem/emoji-candidates/upload"
	request, err := http.NewRequest(http.MethodPost, endpoint, &body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	setBridgeAuthorization(request, job.Config.Token)
	response, err := doEmojiCandidateRequest(request, emojiHTTPTimeout(job.Config))
	if err != nil {
		return err
	}
	if response.Outcome != "pending" && response.Outcome != "known" && response.Outcome != "disabled" {
		return fmt.Errorf("unexpected PawzoChat emoji outcome: %s", response.Outcome)
	}
	return nil
}

func emojiCandidateFields(job emojiCollectionJob) map[string]string {
	return map[string]string{
		"persona_id": job.PersonaID, "session_key": job.SessionKey,
		"session_name": job.SessionName, "sender_id": job.SenderID,
		"sender_name": job.SenderName, "wechat_md5": job.WeChatMD5,
	}
}

func setBridgeAuthorization(request *http.Request, token string) {
	if token = strings.TrimSpace(token); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
}

func doEmojiCandidateRequest(request *http.Request, timeout time.Duration) (emojiCandidateResponse, error) {
	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return emojiCandidateResponse{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil {
		return emojiCandidateResponse{}, err
	}
	if len(body) > 1024*1024 {
		return emojiCandidateResponse{}, errors.New("PawzoChat emoji response exceeds 1 MiB")
	}
	var decoded emojiCandidateResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return emojiCandidateResponse{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if decoded.Error == "" {
			decoded.Error = response.Status
		}
		return emojiCandidateResponse{}, errors.New(decoded.Error)
	}
	return decoded, nil
}

func emojiHTTPTimeout(config Config) time.Duration {
	timeout := time.Duration(config.HTTPTimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > 15*time.Second {
		return 15 * time.Second
	}
	return timeout
}

func (p *PawzoChatPlugin) downloadEmoji(job emojiCollectionJob) ([]byte, string, string, error) {
	return resolveEmojiDownload(job, p.cdn, p.emojiHTTPClient)
}

func resolveEmojiDownload(
	job emojiCollectionJob,
	downloader imageCDNDownloader,
	httpClient *http.Client,
) ([]byte, string, string, error) {
	startedAt := time.Now()
	if len(job.Data) > 0 {
		contentType, extension, err := validateEmojiBytes(job.Data)
		if err == nil {
			logEmojiDownloadSuccess(job.WeChatMD5, "inline", startedAt)
			return job.Data, contentType, extension, nil
		}
	}
	var directErr error
	if job.CDNURL != "" {
		raw, contentType, extension, err := downloadEmojiURL(job.CDNURL, emojiMediaHTTPTimeout, httpClient)
		if err == nil {
			logEmojiDownloadSuccess(job.WeChatMD5, "direct_url", startedAt)
			return raw, contentType, extension, nil
		}
		directErr = fmt.Errorf("direct WeChat emoji download: %w", err)
	}
	var cdnErr error
	if downloader != nil && job.FileID != "" && job.AESKey != "" {
		reader, err := downloadEmojiCDN(context.Background(), downloader, job.FileID, job.AESKey, emojiMediaCDNTimeout)
		if err == nil && reader != nil {
			raw, readErr := readLimitedEmoji(reader)
			if readErr == nil {
				contentType, extension, validErr := validateEmojiBytes(raw)
				if validErr == nil {
					logEmojiDownloadSuccess(job.WeChatMD5, "cdn_ability", startedAt)
					return raw, contentType, extension, nil
				}
				cdnErr = validErr
			} else {
				cdnErr = readErr
			}
		} else if err != nil {
			cdnErr = err
		} else {
			cdnErr = errors.New("CDN downloader returned a nil reader")
		}
	}
	if directErr != nil || cdnErr != nil {
		return nil, "", "", errors.Join(directErr, cdnErr)
	}
	return nil, "", "", errors.New("emoji is missing downloadable media")
}

func logEmojiDownloadSuccess(wechatMD5 string, source string, startedAt time.Time) {
	slog.Info("[pawzochat] 表情包候选下载完成", "wechat_md5", wechatMD5,
		"source", source, "elapsed_ms", time.Since(startedAt).Milliseconds())
}

type imageCDNDownloader interface {
	DownloadImage(fileID, fileAesKey string) (io.ReadCloser, error)
}

type imageCDNContextDownloader interface {
	DownloadImageContext(context.Context, string, string) (io.ReadCloser, error)
}

func downloadEmojiCDN(
	ctx context.Context,
	downloader imageCDNDownloader,
	fileID string,
	fileAesKey string,
	timeout time.Duration,
) (io.ReadCloser, error) {
	cdnCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if contextual, ok := downloader.(imageCDNContextDownloader); ok {
		return contextual.DownloadImageContext(cdnCtx, fileID, fileAesKey)
	}

	type result struct {
		reader io.ReadCloser
		err    error
	}
	resultCh := make(chan result)
	go func() {
		reader, err := downloader.DownloadImage(fileID, fileAesKey)
		select {
		case resultCh <- result{reader: reader, err: err}:
		case <-cdnCtx.Done():
			if reader != nil {
				_ = reader.Close()
			}
		}
	}()
	select {
	case value := <-resultCh:
		return value.reader, value.err
	case <-cdnCtx.Done():
		return nil, cdnCtx.Err()
	}
}

func readLimitedEmoji(reader io.ReadCloser) ([]byte, error) {
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, maxCollectedEmojiBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCollectedEmojiBytes {
		return nil, errors.New("emoji exceeds 8 MiB")
	}
	return raw, nil
}

func downloadEmojiURL(rawURL string, timeout time.Duration, baseClient *http.Client) ([]byte, string, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || !allowedEmojiCDNURL(parsed) {
		return nil, "", "", errors.New("emoji CDN URL is not allowed")
	}
	client := &http.Client{}
	if baseClient != nil {
		*client = *baseClient
	}
	client.Timeout = timeout
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many emoji CDN redirects")
		}
		if !allowedEmojiCDNURL(request.URL) {
			return errors.New("emoji CDN redirect is not allowed")
		}
		return nil
	}
	request, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", "", err
	}
	request.Header.Set("User-Agent", "PawzoChat-Golem/0.5")
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, "", "", errors.New("emoji CDN request timed out")
		}
		return nil, "", "", errors.New("emoji CDN request failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, "", "", fmt.Errorf("emoji CDN returned %s", response.Status)
	}
	raw, err := readLimitedEmoji(response.Body)
	if err != nil {
		return nil, "", "", err
	}
	contentType, extension, err := validateEmojiBytes(raw)
	if err != nil {
		return nil, "", "", err
	}
	return raw, contentType, extension, nil
}

func allowedEmojiCDNURL(value *url.URL) bool {
	if value == nil || (value.Scheme != "https" && value.Scheme != "http") || value.User != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(value.Hostname(), "."))
	for _, domain := range []string{"qq.com", "qpic.cn", "weixin.qq.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func validateEmojiBytes(raw []byte) (string, string, error) {
	if len(raw) == 0 || len(raw) > maxCollectedEmojiBytes {
		return "", "", errors.New("invalid emoji size")
	}
	contentType, extension := "", ""
	var width, height int
	switch {
	case len(raw) >= 8 && bytes.Equal(raw[:8], []byte("\x89PNG\r\n\x1a\n")):
		contentType, extension = "image/png", ".png"
	case len(raw) >= 3 && bytes.Equal(raw[:3], []byte("\xff\xd8\xff")):
		contentType, extension = "image/jpeg", ".jpg"
	case len(raw) >= 6 && (bytes.Equal(raw[:6], []byte("GIF87a")) || bytes.Equal(raw[:6], []byte("GIF89a"))):
		contentType, extension = "image/gif", ".gif"
	case len(raw) >= 12 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP":
		contentType, extension = "image/webp", ".webp"
		width, height = webPDimensions(raw)
	default:
		return "", "", errors.New("unsupported emoji image format")
	}
	if contentType != "image/webp" {
		config, _, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return "", "", errors.New("invalid emoji image")
		}
		width, height = config.Width, config.Height
	}
	if width <= 0 || height <= 0 || int64(width)*int64(height) > maxCollectedEmojiPixels {
		return "", "", errors.New("invalid emoji dimensions")
	}
	return contentType, extension, nil
}

func webPDimensions(raw []byte) (int, int) {
	if len(raw) < 20 || string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WEBP" {
		return 0, 0
	}
	declaredSize := uint64(binary.LittleEndian.Uint32(raw[4:8])) + 8
	if declaredSize > uint64(len(raw)) {
		return 0, 0
	}
	width, height := 0, 0
	animated, hasImage, hasAnimationFrame := false, false, false
	for offset := 12; offset+8 <= len(raw); {
		chunkType := string(raw[offset : offset+4])
		chunkSize := uint64(binary.LittleEndian.Uint32(raw[offset+4 : offset+8]))
		payloadStart := offset + 8
		payloadEnd := uint64(payloadStart) + chunkSize
		if payloadEnd > uint64(len(raw)) {
			return 0, 0
		}
		payload := raw[payloadStart:int(payloadEnd)]
		switch chunkType {
		case "VP8X":
			if len(payload) < 10 {
				return 0, 0
			}
			animated = payload[0]&0x02 != 0
			width = 1 + int(payload[4]) + int(payload[5])<<8 + int(payload[6])<<16
			height = 1 + int(payload[7]) + int(payload[8])<<8 + int(payload[9])<<16
		case "VP8 ":
			if len(payload) < 10 || !bytes.Equal(payload[3:6], []byte("\x9d\x01\x2a")) {
				return 0, 0
			}
			hasImage = true
			width = int(binary.LittleEndian.Uint16(payload[6:8]) & 0x3fff)
			height = int(binary.LittleEndian.Uint16(payload[8:10]) & 0x3fff)
		case "VP8L":
			if len(payload) < 5 || payload[0] != 0x2f {
				return 0, 0
			}
			hasImage = true
			bits := binary.LittleEndian.Uint32(payload[1:5])
			width = int(bits&0x3fff) + 1
			height = int((bits>>14)&0x3fff) + 1
		case "ANMF":
			hasAnimationFrame = true
		}
		advance := 8 + chunkSize
		if advance&1 != 0 {
			advance++
		}
		if advance > uint64(len(raw)-offset) {
			return 0, 0
		}
		offset += int(advance)
	}
	if width <= 0 || height <= 0 || (animated && !hasAnimationFrame) || (!animated && !hasImage) {
		return 0, 0
	}
	return width, height
}
