// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// maxJobAssetBytes bounds the assets of one job on the worker (#624).
const maxJobAssetBytes = 256 << 20

// fetchAssets downloads the job assets into directory/assets before ffmpeg
// starts: the subtitle at the top, the fonts in fonts/. It returns the asset
// directory.
func (job *agentJob) fetchAssets(ctx context.Context, directory string) (string, error) {
	assetDir := filepath.Join(directory, "assets")
	if err := os.MkdirAll(filepath.Join(assetDir, "fonts"), 0o700); err != nil {
		return "", err
	}
	var total int64
	for _, asset := range job.spec.Assets {
		if !assetName.MatchString(asset.Name) || asset.Size < 0 {
			return "", fmt.Errorf("asset name %q is invalid", asset.Name)
		}
		total += asset.Size
		if total > maxJobAssetBytes {
			return "", errors.New("job assets exceed the size limit")
		}
		target := filepath.Join(assetDir, asset.Name)
		if fontAsset(asset.Name) {
			target = filepath.Join(assetDir, "fonts", asset.Name)
		}
		if err := job.fetchAsset(ctx, asset, target); err != nil {
			return "", fmt.Errorf("%s: %w", asset.Name, err)
		}
	}
	return assetDir, nil
}

func (job *agentJob) fetchAsset(ctx context.Context, asset JobAsset, target string) error {
	location := *job.agent.base
	location.Path += pathJobs + job.spec.ID + "/assets/" + asset.Name
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+job.spec.Token)
	response, err := job.agent.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET answered %s", response.Status)
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, asset.Size+1))
	closeErr := file.Close()
	switch {
	case copyErr != nil:
		return copyErr
	case closeErr != nil:
		return closeErr
	case written != asset.Size:
		return fmt.Errorf("got %d bytes, want %d", written, asset.Size)
	}
	return nil
}
