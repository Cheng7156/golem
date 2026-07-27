package sticker

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golem_plugin_hermes/internal/domain"
	storeport "golem_plugin_hermes/internal/store"
)

const (
	LocalLibraryProviderID     = "golem_local_library"
	maxLibraryDescriptionRunes = 300
	maxLibrarySearchPool       = 80
)

var (
	ErrLibraryStorageFull    = errors.New("sticker library storage budget is exhausted")
	ErrCollectionForbidden   = errors.New("sticker collection is not allowed for this principal")
	ErrManagementForbidden   = errors.New("sticker library management is not allowed for this principal")
	ErrRecentStickerNotFound = errors.New("recent sent sticker is not in the local library")
)

type LibraryRepository interface {
	StoreStickerCollection(
		context.Context,
		domain.StickerAsset,
		domain.StickerLabel,
		[]domain.StickerSearchTerm,
		int64,
	) (domain.StickerCollectionResult, error)
	GetStickerAsset(context.Context, string) (domain.StickerAsset, error)
	StickerLibraryStorageBytes(context.Context) (int64, error)
	SearchStickerLibrary(
		context.Context,
		[]domain.StickerSearchTerm,
		int,
	) ([]domain.StickerLibraryMatch, error)
	StickerLibraryInventory(
		context.Context,
		int,
		int,
	) ([]domain.StickerLibraryInventoryItem, int, error)
	FindRecentSentStickerAsset(context.Context, string) (domain.StickerAsset, error)
	ReplaceStickerDescription(
		context.Context,
		domain.StickerLabel,
		[]domain.StickerSearchTerm,
	) error
	DeleteStickerAsset(context.Context, string) error
}

type LibraryConfig struct {
	Directory        string
	MaxStorageBytes  int64
	MaxMediaBytes    int64
	CollectionPolicy string
}

type LibraryCollectRequest struct {
	Description       string
	Data              []byte
	MIMEType          string
	SourceSessionID   string
	SourceEventID     string
	SourceMessageID   string
	SourceSpeakerID   string
	SourceSpeakerName string
	Collector         domain.Principal
}

type LibraryCollectResult struct {
	StickerID    string
	Description  string
	AssetCreated bool
	LabelCreated bool
}

type LibraryInventory struct {
	Items  []domain.StickerLibraryInventoryItem
	Total  int
	Limit  int
	Offset int
}

type LibraryManageRequest struct {
	Action      string
	Description string
	SessionID   string
	Principal   domain.Principal
}

type LibraryManageResult struct {
	Action      string
	Description string
}

type LocalLibrary struct {
	config     LibraryConfig
	repository LibraryRepository
	mu         sync.Mutex
	now        func() time.Time
}

func NewLocalLibrary(config LibraryConfig, repository LibraryRepository) (*LocalLibrary, error) {
	config.Directory = filepath.Clean(strings.TrimSpace(config.Directory))
	config.CollectionPolicy = strings.ToLower(strings.TrimSpace(config.CollectionPolicy))
	if config.Directory == "." || config.MaxStorageBytes <= 0 || config.MaxMediaBytes <= 0 {
		return nil, errors.New("sticker library configuration is invalid")
	}
	if config.MaxStorageBytes < config.MaxMediaBytes {
		return nil, errors.New("sticker library storage budget is smaller than one media item")
	}
	if config.CollectionPolicy != "owner" && config.CollectionPolicy != "any" {
		return nil, errors.New("sticker library collection policy is invalid")
	}
	if repository == nil {
		return nil, errors.New("sticker library repository is required")
	}
	if err := os.MkdirAll(config.Directory, 0o750); err != nil {
		return nil, err
	}
	return &LocalLibrary{config: config, repository: repository, now: time.Now}, nil
}

func (l *LocalLibrary) ID() string { return LocalLibraryProviderID }

func (l *LocalLibrary) AuthorizeCollection(principal domain.Principal) error {
	if l.config.CollectionPolicy == "owner" && !principal.IsOwner {
		return ErrCollectionForbidden
	}
	return nil
}

func (l *LocalLibrary) AuthorizeManagement(principal domain.Principal) error {
	if !principal.IsOwner {
		return ErrManagementForbidden
	}
	return nil
}

func (l *LocalLibrary) Collect(
	ctx context.Context,
	request LibraryCollectRequest,
) (LibraryCollectResult, error) {
	if err := ctx.Err(); err != nil {
		return LibraryCollectResult{}, err
	}
	if err := l.AuthorizeCollection(request.Collector); err != nil {
		return LibraryCollectResult{}, err
	}
	description := strings.TrimSpace(request.Description)
	if description == "" || len([]rune(description)) > maxLibraryDescriptionRunes {
		return LibraryCollectResult{}, errors.New("sticker description is empty or too long")
	}
	if len(request.Data) == 0 || int64(len(request.Data)) > l.config.MaxMediaBytes {
		return LibraryCollectResult{}, errors.New("sticker media is empty or too large")
	}
	mimeType, err := imageMIME(request.Data)
	if err != nil {
		return LibraryCollectResult{}, err
	}
	if supplied := strings.ToLower(strings.TrimSpace(request.MIMEType)); supplied != "" && supplied != mimeType {
		return LibraryCollectResult{}, errors.New("sticker media MIME does not match its bytes")
	}
	digestBytes := sha256.Sum256(request.Data)
	digest := hex.EncodeToString(digestBytes[:])
	path, err := libraryAssetPath(l.config.Directory, digest, mimeType)
	if err != nil {
		return LibraryCollectResult{}, err
	}
	now := l.now().UTC()
	asset := domain.StickerAsset{
		ID: "stkl_" + digest, MIMEType: mimeType, Path: path,
		Size: int64(len(request.Data)), SHA256: digest, CreatedAt: now,
	}
	label := domain.StickerLabel{
		StickerID: asset.ID, Description: description,
		DescriptionNorm: normalizeLibraryText(description),
		SourceSessionID: request.SourceSessionID, SourceEventID: request.SourceEventID,
		SourceMessageID: request.SourceMessageID, SourceSpeakerID: request.SourceSpeakerID,
		SourceSpeakerName: request.SourceSpeakerName, CollectedByID: request.Collector.ID,
		CollectedByName: request.Collector.Name, CreatedAt: now,
	}
	if label.DescriptionNorm == "" {
		return LibraryCollectResult{}, errors.New("sticker description contains no searchable text")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	existing, getErr := l.repository.GetStickerAsset(ctx, asset.ID)
	assetExists := getErr == nil
	if getErr != nil && !errors.Is(getErr, storeport.ErrNotFound) {
		return LibraryCollectResult{}, getErr
	}
	if assetExists {
		if existing.Size != asset.Size || existing.SHA256 != asset.SHA256 ||
			existing.MIMEType != asset.MIMEType || filepath.Clean(existing.Path) != path {
			return LibraryCollectResult{}, errors.New("sticker library asset collision")
		}
		asset = existing
	} else {
		total, totalErr := l.repository.StickerLibraryStorageBytes(ctx)
		if totalErr != nil {
			return LibraryCollectResult{}, totalErr
		}
		if total+asset.Size > l.config.MaxStorageBytes {
			return LibraryCollectResult{}, ErrLibraryStorageFull
		}
	}
	fileCreated, err := ensureLibraryFile(l.config.Directory, path, request.Data, digest)
	if err != nil {
		return LibraryCollectResult{}, err
	}
	stored, err := l.repository.StoreStickerCollection(
		ctx, asset, label, librarySearchTerms(description), l.config.MaxStorageBytes,
	)
	if err != nil {
		if !assetExists && fileCreated {
			_ = os.Remove(path)
		}
		if errors.Is(err, storeport.ErrCapacity) {
			return LibraryCollectResult{}, ErrLibraryStorageFull
		}
		return LibraryCollectResult{}, err
	}
	return LibraryCollectResult{
		StickerID: stored.Asset.ID, Description: description,
		AssetCreated: stored.AssetCreated, LabelCreated: stored.LabelCreated,
	}, nil
}

func (l *LocalLibrary) Search(
	ctx context.Context,
	request ProviderSearchRequest,
) ([]ProviderCandidate, error) {
	if request.Page > 1 {
		return nil, nil
	}
	query := strings.TrimSpace(request.Query)
	terms := librarySearchTerms(query)
	if len(terms) == 0 {
		return nil, errors.New("sticker library query is empty")
	}
	limit := request.Limit
	if limit <= 0 {
		limit = 5
	}
	poolSize := min(maxLibrarySearchPool, max(limit*8, limit))
	matches, err := l.repository.SearchStickerLibrary(ctx, terms, poolSize)
	if err != nil {
		return nil, err
	}
	matches = relevantLibraryMatches(matches, query)
	shuffleLibraryMatches(matches)
	if len(matches) > limit {
		matches = matches[:limit]
	}
	result := make([]ProviderCandidate, 0, len(matches))
	for _, match := range matches {
		result = append(result, ProviderCandidate{
			Reference: match.StickerID, Description: match.Description,
		})
	}
	return result, nil
}

func (l *LocalLibrary) Inventory(
	ctx context.Context,
	limit int,
	offset int,
) (LibraryInventory, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 || offset < 0 {
		return LibraryInventory{}, errors.New("sticker library inventory range is invalid")
	}
	items, total, err := l.repository.StickerLibraryInventory(ctx, limit, offset)
	if err != nil {
		return LibraryInventory{}, err
	}
	return LibraryInventory{Items: items, Total: total, Limit: limit, Offset: offset}, nil
}

func (l *LocalLibrary) ManageRecent(
	ctx context.Context,
	request LibraryManageRequest,
) (LibraryManageResult, error) {
	if err := ctx.Err(); err != nil {
		return LibraryManageResult{}, err
	}
	if err := l.AuthorizeManagement(request.Principal); err != nil {
		return LibraryManageResult{}, err
	}
	action := strings.TrimSpace(request.Action)
	description := strings.TrimSpace(request.Description)
	if strings.TrimSpace(request.SessionID) == "" {
		return LibraryManageResult{}, errors.New("sticker management session is empty")
	}
	if action != "update_description" && action != "delete" {
		return LibraryManageResult{}, errors.New("sticker management action is invalid")
	}
	if action == "delete" && description != "" {
		return LibraryManageResult{}, errors.New("delete does not accept a description")
	}
	normalized := normalizeLibraryText(description)
	if action == "update_description" && (description == "" ||
		len([]rune(description)) > maxLibraryDescriptionRunes || normalized == "") {
		return LibraryManageResult{}, errors.New("sticker description is empty or too long")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	asset, err := l.repository.FindRecentSentStickerAsset(ctx, strings.TrimSpace(request.SessionID))
	if errors.Is(err, storeport.ErrNotFound) {
		return LibraryManageResult{}, ErrRecentStickerNotFound
	}
	if err != nil {
		return LibraryManageResult{}, err
	}
	if action == "update_description" {
		label := domain.StickerLabel{
			StickerID: asset.ID, Description: description, DescriptionNorm: normalized,
			SourceSessionID: request.SessionID, CollectedByID: request.Principal.ID,
			CollectedByName: request.Principal.Name, CreatedAt: l.now().UTC(),
		}
		if err := l.repository.ReplaceStickerDescription(
			ctx, label, librarySearchTerms(description),
		); err != nil {
			if errors.Is(err, storeport.ErrNotFound) {
				return LibraryManageResult{}, ErrRecentStickerNotFound
			}
			return LibraryManageResult{}, err
		}
		return LibraryManageResult{Action: action, Description: description}, nil
	}
	if err := l.deleteAsset(ctx, asset); err != nil {
		if errors.Is(err, storeport.ErrNotFound) {
			return LibraryManageResult{}, ErrRecentStickerNotFound
		}
		return LibraryManageResult{}, err
	}
	return LibraryManageResult{Action: action}, nil
}

func relevantLibraryMatches(
	values []domain.StickerLibraryMatch,
	query string,
) []domain.StickerLibraryMatch {
	if len(values) == 0 {
		return nil
	}
	minimum := minimumLibraryScore(query)
	cutoff := max(minimum, values[0].Score*3/5)
	result := values[:0]
	for _, value := range values {
		if value.Score < cutoff {
			continue
		}
		result = append(result, value)
	}
	return result
}

func shuffleLibraryMatches(values []domain.StickerLibraryMatch) {
	for index := len(values) - 1; index > 0; index-- {
		selected, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(index+1)))
		if err != nil {
			return
		}
		other := int(selected.Int64())
		values[index], values[other] = values[other], values[index]
	}
}

func (l *LocalLibrary) Materialize(
	ctx context.Context,
	candidate ProviderCandidate,
) (domain.EmojiOutput, error) {
	asset, err := l.repository.GetStickerAsset(ctx, strings.TrimSpace(candidate.Reference))
	if err != nil {
		return domain.EmojiOutput{}, err
	}
	data, mimeType, err := readLibraryFile(l.config.Directory, asset, l.config.MaxMediaBytes)
	if err != nil {
		return domain.EmojiOutput{}, err
	}
	return domain.EmojiOutput{
		Data: data, MIMEType: mimeType, Description: strings.TrimSpace(candidate.Description),
	}, nil
}
