// SPDX-License-Identifier: GPL-2.0-only

package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/deadfin-org/deadfin/internal/jsonnum"
)

// Named configuration keys. Each matches the ConfigurationStore.Key that an
// upstream IConfigurationFactory registers, and the lower-case file name that
// BaseConfigurationManager.GetConfigurationFile derives from it
// (BaseConfigurationManager.cs:271-274):
//   - branding: Emby.Server.Implementations/Branding/BrandingConfigurationFactory.cs
//   - network: MediaBrowser.Common/Net/NetworkConfigurationStore.cs
//   - metadata: MediaBrowser.Controller/Library/MetadataConfigurationStore.cs
//   - encoding: MediaBrowser.MediaEncoding/Configuration/EncodingConfigurationStore.cs
//   - livetv: src/Jellyfin.LiveTv/Configuration/LiveTvConfigurationFactory.cs
//   - xbmcmetadata: MediaBrowser.XbmcMetadata/Configuration/NfoConfigurationFactory.cs
const (
	KeyBranding = "branding"
	KeyNetwork  = "network"
	KeyMetadata = "metadata"
	KeyEncoding = "encoding"
	KeyLiveTv   = "livetv"

	KeyXbmcMetadata = "xbmcmetadata"
)

// Files that a Change can name.
const (
	SystemFile   = systemFile
	BrandingFile = KeyBranding + ".xml"
	NetworkFile  = KeyNetwork + ".xml"
	MetadataFile = KeyMetadata + ".xml"
	EncodingFile = KeyEncoding + ".xml"
	LiveTvFile   = KeyLiveTv + ".xml"

	XbmcMetadataFile = KeyXbmcMetadata + ".xml"
)

// ErrUnknownConfiguration reports a named configuration key that no store
// registers. Upstream throws ResourceNotFoundException (404)
// (BaseConfigurationManager.cs:291-294).
var ErrUnknownConfiguration = errors.New("config: configuration not found")

// ErrDirectoryNotFound reports a new configured path that does not exist.
// Upstream throws DirectoryNotFoundException (404)
// (ServerConfigurationManager.cs:101-118, BaseConfigurationManager.cs:240-256,
// EncodingConfigurationStore.cs:27-40).
var ErrDirectoryNotFound = errors.New("config: directory does not exist")

// ErrEncoderAppPathLocked reports a posted encoding configuration that changes
// EncoderAppPath. Upstream throws InvalidOperationException
// ("Unable to update encoder app path.") (EncodingConfigurationStore.cs:42-46).
var ErrEncoderAppPathLocked = errors.New("config: unable to update encoder app path")

// NamedConfiguration returns the current value of a named configuration as
// GetNamedConfiguration serializes it. The key is case-insensitive, as
// upstream's store lookup is (BaseConfigurationManager.cs:286-289).
func (manager *Manager) NamedConfiguration(key string) (any, error) {
	switch strings.ToLower(key) {
	case KeyBranding:
		return manager.Branding(), nil
	case KeyNetwork:
		return manager.Network(), nil
	case KeyMetadata:
		return manager.Metadata(), nil
	case KeyEncoding:
		return manager.Encoding(), nil
	case KeyLiveTv:
		return manager.LiveTv(), nil
	case KeyXbmcMetadata:
		return manager.XbmcMetadata(), nil
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownConfiguration, key)
}

// SaveNamedConfiguration decodes a posted named configuration over the
// upstream defaults of its registered type, validates it, and persists it.
// It ports ConfigurationController.UpdateNamedConfiguration and
// BaseConfigurationManager.SaveConfiguration(key, object)
// (ConfigurationController.cs:92-106, BaseConfigurationManager.cs:323-353).
//
// The branding key keeps SplashscreenLocation. Upstream routes a POST to
// /System/Configuration/branding to UpdateBrandingConfiguration because
// ASP.NET prefers the literal Branding segment and matches it
// case-insensitively (ConfigurationController.cs:122-137).
func (manager *Manager) SaveNamedConfiguration(ctx context.Context, key string, data []byte) error {
	switch strings.ToLower(key) {
	case KeyBranding:
		var posted BrandingOptionsUpdate
		if err := decodeJSONOverDefaults(data, &posted); err != nil {
			return err
		}
		return manager.UpdateBrandingOptions(ctx, posted)
	case KeyNetwork:
		var posted NetworkConfiguration
		if err := jsonnum.Unmarshal(data, &posted); err != nil {
			return invalidConfiguration(err)
		}
		return manager.UpdateNetwork(ctx, func(network *NetworkConfiguration) error {
			*network = posted
			return nil
		})
	case KeyMetadata:
		var posted MetadataConfiguration
		if err := jsonnum.Unmarshal(data, &posted); err != nil {
			return invalidConfiguration(err)
		}
		return manager.UpdateMetadata(ctx, func(metadata *MetadataConfiguration) error {
			*metadata = posted
			return nil
		})
	case KeyEncoding:
		var posted EncodingOptions
		if err := jsonnum.Unmarshal(data, &posted); err != nil {
			return invalidConfiguration(err)
		}
		return manager.UpdateEncoding(ctx, func(options *EncodingOptions) error {
			if err := validateEncodingOptions(*options, posted); err != nil {
				return err
			}
			if posted.AllowPlainHttpTranscodeWorkers == nil && options.AllowPlainHttpTranscodeWorkers != nil {
				value := *options.AllowPlainHttpTranscodeWorkers
				posted.AllowPlainHttpTranscodeWorkers = &value
			}
			*options = posted
			return nil
		})
	case KeyLiveTv:
		var posted LiveTvOptions
		if err := posted.UnmarshalJSON(data); err != nil {
			return err
		}
		return manager.UpdateLiveTv(ctx, func(options *LiveTvOptions) error {
			posted.keepDeadfinFields(*options)
			*options = posted
			return nil
		})
	case KeyXbmcMetadata:
		var posted XbmcMetadataOptions
		if err := jsonnum.Unmarshal(data, &posted); err != nil {
			return invalidConfiguration(err)
		}
		return manager.UpdateXbmcMetadata(ctx, func(options *XbmcMetadataOptions) error {
			*options = posted
			return nil
		})
	}
	return fmt.Errorf("%w: %s", ErrUnknownConfiguration, key)
}

// ReplaceSystem validates and persists a complete server configuration. It
// ports ServerConfigurationManager.ReplaceConfiguration and
// BaseConfigurationManager.ReplaceConfiguration: a changed, non-blank
// MetadataPath or CachePath must exist and be writable
// (ServerConfigurationManager.cs:86-118, BaseConfigurationManager.cs:187-256).
// A completed startup wizard stays completed (D-46).
func (manager *Manager) ReplaceSystem(ctx context.Context, configuration ServerConfiguration) error {
	configuration.normalizeArrays()
	return manager.UpdateSystem(ctx, func(current *ServerConfiguration) error {
		if err := validateNewDirectory(current.MetadataPath, configuration.MetadataPath); err != nil {
			return err
		}
		if err := validateNewDirectory(stringValue(current.CachePath), stringValue(configuration.CachePath)); err != nil {
			return err
		}
		// A save never reopens the anonymous startup endpoints (D-46).
		if current.IsStartupWizardCompleted {
			configuration.IsStartupWizardCompleted = true
		}
		*current = configuration
		return nil
	})
}

// BrandingOptionsUpdate is the BrandingOptionsDto that
// UpdateBrandingConfiguration accepts. It omits SplashscreenLocation, which
// only the splash screen upload and delete operations change
// (upstream/jellyfin/MediaBrowser.Model/Branding/BrandingOptionsDto.cs:6-25).
type BrandingOptionsUpdate struct {
	LoginDisclaimer     *string
	CustomCss           *string
	SplashscreenEnabled bool
}

// UpdateBrandingOptions applies a BrandingOptionsDto to the current branding
// configuration and keeps SplashscreenLocation
// (ConfigurationController.cs:122-137).
func (manager *Manager) UpdateBrandingOptions(ctx context.Context, update BrandingOptionsUpdate) error {
	return manager.UpdateBranding(ctx, func(branding *BrandingOptions) error {
		branding.LoginDisclaimer = stringValue(update.LoginDisclaimer)
		branding.CustomCss = stringValue(update.CustomCss)
		branding.SplashscreenEnabled = update.SplashscreenEnabled
		return nil
	})
}

// UnmarshalJSON decodes a BrandingOptionsDto over its defaults.
func (update *BrandingOptionsUpdate) UnmarshalJSON(data []byte) error {
	type fields BrandingOptionsUpdate
	var decoded fields
	if err := decodeJSONOverDefaults(data, &decoded); err != nil {
		return err
	}
	*update = BrandingOptionsUpdate(decoded)
	return nil
}

// UpdateMetadata serializes a durable mutation of metadata.xml.
func (manager *Manager) UpdateMetadata(ctx context.Context, update func(*MetadataConfiguration) error) error {
	return updateConfiguration(ctx, manager, &manager.metadata, MetadataFile, KeyMetadata, update)
}

// UpdateEncoding serializes a durable mutation of encoding.xml.
func (manager *Manager) UpdateEncoding(ctx context.Context, update func(*EncodingOptions) error) error {
	return updateConfiguration(ctx, manager, &manager.encoding, EncodingFile, KeyEncoding, update)
}

// validateEncodingOptions ports EncodingConfigurationStore.Validate
// (EncodingConfigurationStore.cs:20-47).
func validateEncodingOptions(current, next EncodingOptions) error {
	if strings.TrimSpace(next.TranscodingTempPath) != "" && current.TranscodingTempPath != next.TranscodingTempPath {
		if info, err := os.Stat(next.TranscodingTempPath); err != nil || !info.IsDir() {
			return fmt.Errorf("%w: %s does not exist.", ErrDirectoryNotFound, next.TranscodingTempPath)
		}
	}
	if strings.TrimSpace(next.EncoderAppPath) != "" && current.EncoderAppPath != next.EncoderAppPath {
		return ErrEncoderAppPathLocked
	}
	return nil
}

// validateNewDirectory ports ValidateMetadataPath and ValidateCachePath: a
// changed, non-blank path must exist and accept a probe file
// (EnsureWriteAccess, BaseConfigurationManager.cs:262-267).
func validateNewDirectory(current, next string) error {
	if strings.TrimSpace(next) == "" || current == next {
		return nil
	}
	if info, err := os.Stat(next); err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s does not exist.", ErrDirectoryNotFound, next)
	}
	probe, err := os.CreateTemp(next, ".deadfin-write-probe-*")
	if err != nil {
		return fmt.Errorf("config: %s is not writable: %w", next, err)
	}
	name := probe.Name()
	return errors.Join(probe.Close(), os.Remove(filepath.Clean(name)))
}

func invalidConfiguration(err error) error {
	if errors.Is(err, ErrInvalidConfiguration) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
