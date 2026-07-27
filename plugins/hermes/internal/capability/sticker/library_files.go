package sticker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

func libraryAssetPath(root, digest, mimeType string) (string, error) {
	extension, err := libraryExtension(mimeType)
	if err != nil {
		return "", err
	}
	if len(digest) != sha256.Size*2 {
		return "", errors.New("sticker library digest is invalid")
	}
	return filepath.Join(root, digest[:2], digest+extension), nil
}

func libraryExtension(mimeType string) (string, error) {
	switch mimeType {
	case "image/jpeg":
		return ".jpg", nil
	case "image/png":
		return ".png", nil
	case "image/gif":
		return ".gif", nil
	case "image/webp":
		return ".webp", nil
	case "image/bmp":
		return ".bmp", nil
	default:
		return "", fmt.Errorf("unsupported sticker library MIME type %q", mimeType)
	}
}

func ensureLibraryFile(root, path string, data []byte, digest string) (bool, error) {
	if existing, _, err := readLibraryFile(root, domain.StickerAsset{
		Path: path, Size: int64(len(data)), SHA256: digest,
	}, int64(len(data))); err == nil && len(existing) == len(data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, fmt.Errorf("create sticker library shard: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".incoming-*")
	if err != nil {
		return false, fmt.Errorf("create sticker library temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("write sticker library file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("sync sticker library file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return false, fmt.Errorf("close sticker library file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return false, fmt.Errorf("install sticker library file: %w", err)
	}
	return true, nil
}

func readLibraryFile(
	root string,
	asset domain.StickerAsset,
	maximum int64,
) ([]byte, string, error) {
	if maximum <= 0 || asset.Size <= 0 || asset.Size > maximum {
		return nil, "", errors.New("sticker library asset exceeds the configured size limit")
	}
	root = filepath.Clean(root)
	path := filepath.Clean(strings.TrimSpace(asset.Path))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, "", errors.New("sticker library path escapes its configured directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", fmt.Errorf("stat sticker library asset: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != asset.Size {
		return nil, "", errors.New("sticker library file metadata is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open sticker library asset: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) != asset.Size {
		return nil, "", errors.New("sticker library file size changed")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != asset.SHA256 {
		return nil, "", errors.New("sticker library file digest changed")
	}
	mimeType, err := imageMIME(data)
	if err != nil || (asset.MIMEType != "" && mimeType != asset.MIMEType) {
		return nil, "", errors.New("sticker library file MIME changed")
	}
	return data, mimeType, nil
}

func (l *LocalLibrary) deleteAsset(ctx context.Context, asset domain.StickerAsset) error {
	expectedPath, err := libraryAssetPath(l.config.Directory, asset.SHA256, asset.MIMEType)
	if err != nil || filepath.Clean(asset.Path) != filepath.Clean(expectedPath) {
		return errors.New("sticker library asset path is invalid")
	}
	if _, _, err := readLibraryFile(l.config.Directory, asset, l.config.MaxMediaBytes); err != nil {
		return err
	}
	tombstone, err := os.CreateTemp(filepath.Dir(asset.Path), ".deleting-*")
	if err != nil {
		return fmt.Errorf("create sticker deletion tombstone: %w", err)
	}
	tombstonePath := tombstone.Name()
	if err := tombstone.Close(); err != nil {
		_ = os.Remove(tombstonePath)
		return fmt.Errorf("close sticker deletion tombstone: %w", err)
	}
	if err := os.Remove(tombstonePath); err != nil {
		return fmt.Errorf("prepare sticker deletion tombstone: %w", err)
	}
	if err := os.Rename(asset.Path, tombstonePath); err != nil {
		return fmt.Errorf("stage sticker library deletion: %w", err)
	}
	if err := l.repository.DeleteStickerAsset(ctx, asset.ID); err != nil {
		if restoreErr := os.Rename(tombstonePath, asset.Path); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore sticker library asset: %w", restoreErr))
		}
		return err
	}
	if err := os.Remove(tombstonePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove sticker library tombstone: %w", err)
	}
	return nil
}
