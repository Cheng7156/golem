package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
)

const (
	mediaStorageWorkers = 1
	mediaStorageQueue   = 32
)

type mediaStorageJob struct {
	Config      Config
	PersonaID   string
	SessionKey  string
	SessionName string
	Context     string
	FileID      string
	CDNURL      string
	AESKey      string
	Data        []byte
	done        chan struct{}
}

type mediaStorageResponse struct {
	Outcome string `json:"outcome"`
	ImageID string `json:"image_id"`
	Error   string `json:"error"`
}

func buildMediaStorageJob(
	msg *message.Message,
	self *contact.SelfInfo,
	ownerID string,
	ownerName string,
	config Config,
) (mediaStorageJob, bool) {
	if msg == nil {
		return mediaStorageJob{}, false
	}
	var media *message.Media
	label := "[图片]"
	switch msg.GetType().GetCode() {
	case message.TypeImage.Code:
		media = msg.GetImage().GetMedia()
	case message.TypeEmoji.Code:
		media = msg.GetEmoji().GetMedia()
		label = "[表情]"
	default:
		return mediaStorageJob{}, false
	}
	if media == nil || (len(media.GetData()) == 0 &&
		strings.TrimSpace(media.GetUrl()) == "" &&
		(strings.TrimSpace(media.GetMd5()) == "" || strings.TrimSpace(media.GetKey()) == "")) {
		return mediaStorageJob{}, false
	}
	incoming, ok := buildMediaContext(msg, self, ownerID, ownerName, label)
	if !ok {
		return mediaStorageJob{}, false
	}
	personaID := strings.TrimSpace(config.Routes[incoming.SessionKey])
	if personaID == "" {
		personaID = strings.TrimSpace(config.DefaultPersonaID)
	}
	if personaID == "" {
		return mediaStorageJob{}, false
	}
	return mediaStorageJob{
		Config: config, PersonaID: personaID, SessionKey: incoming.SessionKey,
		SessionName: incoming.sessionName(), Context: incoming.promptContent(),
		FileID: strings.TrimSpace(media.GetMd5()), CDNURL: strings.TrimSpace(media.GetUrl()),
		AESKey: strings.TrimSpace(media.GetKey()), Data: append([]byte(nil), media.GetData()...),
	}, true
}

func (p *PawzoChatPlugin) startMediaWorkers() {
	p.mediaMu.Lock()
	defer p.mediaMu.Unlock()
	if p.mediaQueue != nil {
		return
	}
	p.mediaQueue = make(chan mediaStorageJob, mediaStorageQueue)
	p.mediaStop = make(chan struct{})
	p.mediaPending = make(map[string]chan struct{})
	queue, stop := p.mediaQueue, p.mediaStop
	for range mediaStorageWorkers {
		p.mediaWG.Add(1)
		go func() {
			defer p.mediaWG.Done()
			for {
				select {
				case <-stop:
					return
				case job := <-queue:
					p.processMediaStorage(job)
				}
			}
		}()
	}
}

func (p *PawzoChatPlugin) stopMediaWorkers() {
	p.mediaMu.Lock()
	if p.mediaQueue == nil {
		p.mediaMu.Unlock()
		return
	}
	close(p.mediaStop)
	p.mediaQueue = nil
	p.mediaStop = nil
	p.mediaPending = nil
	p.mediaMu.Unlock()
	p.mediaWG.Wait()
}

func (p *PawzoChatPlugin) enqueueMediaStorage(job mediaStorageJob) {
	p.mediaMu.Lock()
	queue, stop := p.mediaQueue, p.mediaStop
	if queue == nil {
		p.mediaMu.Unlock()
		return
	}
	job.done = make(chan struct{})
	select {
	case <-stop:
		p.mediaMu.Unlock()
		return
	case queue <- job:
		p.mediaPending[job.SessionKey] = job.done
		p.mediaMu.Unlock()
	default:
		p.mediaMu.Unlock()
		close(job.done)
		slog.Warn("[pawzochat] 图片登记队列已满，已跳过")
	}
}

func (p *PawzoChatPlugin) processMediaStorage(job mediaStorageJob) {
	defer p.finishMediaStorage(job)
	download := emojiCollectionJob{
		WeChatMD5: job.FileID, FileID: job.FileID, CDNURL: job.CDNURL,
		AESKey: job.AESKey, Data: job.Data,
	}
	raw, contentType, extension, err := resolveEmojiDownload(download, p.cdn, p.emojiHTTPClient)
	if err != nil {
		slog.Warn("[pawzochat] 下载待登记图片失败", "session_type", sessionType(job.SessionKey), "err", err)
		return
	}
	response, err := uploadStoredMedia(job, raw, contentType, extension)
	if err != nil {
		slog.Warn("[pawzochat] 登记图片失败", "session_type", sessionType(job.SessionKey), "err", err)
		return
	}
	slog.Info("[pawzochat] 图片已登记，等待按需识别", "session_type", sessionType(job.SessionKey), "image_id", response.ImageID)
}

func (p *PawzoChatPlugin) finishMediaStorage(job mediaStorageJob) {
	if job.done == nil {
		return
	}
	p.mediaMu.Lock()
	if p.mediaPending[job.SessionKey] == job.done {
		delete(p.mediaPending, job.SessionKey)
	}
	close(job.done)
	p.mediaMu.Unlock()
}

func (p *PawzoChatPlugin) waitForPendingMedia(sessionKey string) {
	p.mediaMu.Lock()
	done := p.mediaPending[sessionKey]
	stop := p.mediaStop
	p.mediaMu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-stop:
	}
}

func uploadStoredMedia(
	job mediaStorageJob,
	raw []byte,
	contentType string,
	extension string,
) (mediaStorageResponse, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{
		"persona_id": job.PersonaID, "session_key": job.SessionKey,
		"session_name": job.SessionName, "context": job.Context,
	} {
		if err := writer.WriteField(key, value); err != nil {
			return mediaStorageResponse{}, err
		}
	}
	part, err := writer.CreateFormFile("image", "image"+extension)
	if err != nil {
		return mediaStorageResponse{}, err
	}
	if _, err := part.Write(raw); err != nil {
		return mediaStorageResponse{}, err
	}
	if err := writer.Close(); err != nil {
		return mediaStorageResponse{}, err
	}
	endpoint := strings.TrimRight(job.Config.BaseURL, "/") + "/api/bridge/golem/media"
	request, err := http.NewRequest(http.MethodPost, endpoint, &body)
	if err != nil {
		return mediaStorageResponse{}, err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Image-Mime", contentType)
	setBridgeAuthorization(request, job.Config.Token)
	response, err := (&http.Client{Timeout: emojiHTTPTimeout(job.Config)}).Do(request)
	if err != nil {
		return mediaStorageResponse{}, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil {
		return mediaStorageResponse{}, err
	}
	if len(responseBody) > 1024*1024 {
		return mediaStorageResponse{}, errors.New("PawzoChat media response exceeds 1 MiB")
	}
	var decoded mediaStorageResponse
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return mediaStorageResponse{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if decoded.Error == "" {
			decoded.Error = response.Status
		}
		return mediaStorageResponse{}, errors.New(decoded.Error)
	}
	if decoded.Outcome != "stored" || decoded.ImageID == "" {
		return mediaStorageResponse{}, fmt.Errorf("unexpected PawzoChat media outcome: %s", decoded.Outcome)
	}
	return decoded, nil
}
