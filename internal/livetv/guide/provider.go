// SPDX-License-Identifier: GPL-2.0-only

package guide

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/livetv/fetch"
	"github.com/deadfin-org/deadfin/internal/livetv/secret"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

// XMLTVCacheMaxAge is how long a downloaded XMLTV file is reused
// (XmlTvListingsProvider._maxCacheAge).
const XMLTVCacheMaxAge = time.Hour

// ErrEmptyXMLTV reports a downloaded or copied XMLTV file with no bytes.
var ErrEmptyXMLTV = errors.New("xmltv: file is empty")

// XMLTV is the "xmltv" listings provider (XmlTvListingsProvider.cs). Its
// Path is a local file or an HTTP(S) URL, optionally gzip-compressed.
type XMLTV struct {
	fetch    *fetch.Client
	clock    clock.Clock
	cacheDir string
	group    singleflight.Group
}

// NewXMLTV returns the XMLTV provider. cacheDir is the server cache
// directory; downloads go to cacheDir/xmltv/<provider id>.xml.
func NewXMLTV(client *fetch.Client, clk clock.Clock, cacheDir string) *XMLTV {
	return &XMLTV{fetch: client, clock: clk, cacheDir: cacheDir}
}

// Type returns the provider type.
func (*XMLTV) Type() string { return "xmltv" }

// Name returns the provider name.
func (*XMLTV) Name() string { return "XmlTV" }

// Validate ports XmlTvListingsProvider.Validate: a local path must exist.
func (*XMLTV) Validate(info config.ListingsProviderInfo) error {
	if hasPrefixFold(info.Path, "http") {
		return nil
	}
	if _, err := os.Stat(info.Path); err != nil {
		return fmt.Errorf("could not find the XmlTv file specified: %w", err)
	}
	return nil
}

// Language returns the guide language of a provider
// (XmlTvListingsProvider.GetLanguage): PreferredLanguage, else the server's
// PreferredMetadataLanguage.
func Language(info config.ListingsProviderInfo, metadataLanguage string) string {
	if strings.TrimSpace(info.PreferredLanguage) != "" {
		return info.PreferredLanguage
	}
	return metadataLanguage
}

// Channels ports XmlTvListingsProvider.GetChannels.
func (x *XMLTV) Channels(ctx context.Context, info config.ListingsProviderInfo, language string) ([]Channel, error) {
	file, err := x.open(ctx, info)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	channels := []Channel{}
	err = ReadXMLTV(ctx, file, &language, Listings{
		Channel: func(c XMLTVChannel) error {
			number := c.Number
			if strings.TrimSpace(number) == "" {
				number = c.ID
			}
			channels = append(channels, Channel{ID: c.ID, Name: c.DisplayName, Number: number, ImageURL: c.IconURL})
			return nil
		},
	})
	if err != nil {
		return nil, x.redact(info, err)
	}
	return channels, nil
}

// Programs ports XmlTvListingsProvider.GetProgramsAsync for several guide
// channels in one pass. It yields the programmes of the channels in
// channelIDs (compared ignoring case) that overlap [start, end), in file
// order. ChannelID is the id as the file spells it.
func (x *XMLTV) Programs(ctx context.Context, info config.ListingsProviderInfo, language string, channelIDs []string, start, end time.Time, yield func(Program) error) error {
	want := make(map[string]struct{}, len(channelIDs))
	for _, id := range channelIDs {
		if strings.TrimSpace(id) != "" {
			want[foldKey(id)] = struct{}{}
		}
	}
	if len(want) == 0 {
		return nil
	}
	file, err := x.open(ctx, info)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	err = ReadXMLTV(ctx, file, &language, Listings{
		WantProgrammes: func(id string) bool {
			_, ok := want[foldKey(id)]
			return ok
		},
		Programme: func(p XMLTVProgramme) error {
			return yield(MapProgramme(p, info))
		},
		Start: start,
		End:   end,
	})
	if err != nil {
		return x.redact(info, err)
	}
	return nil
}

func (x *XMLTV) redact(info config.ListingsProviderInfo, err error) error {
	return secret.Collect([]string{info.Path}).Err(err)
}

func (x *XMLTV) open(ctx context.Context, info config.ListingsProviderInfo) (*os.File, error) {
	file, err := x.CachePath(ctx, info)
	if err != nil {
		return nil, err
	}
	return os.Open(file)
}

// CachePath ports XmlTvListingsProvider.GetXml: it returns the cached copy
// of the provider's file, downloading or copying it when the copy is
// missing or older than XMLTVCacheMaxAge. Concurrent callers for one
// provider share one download, and the copy appears complete or not at all.
func (x *XMLTV) CachePath(ctx context.Context, info config.ListingsProviderInfo) (string, error) {
	if info.Id == "" || info.Id != filepath.Base(info.Id) || info.Id == ".." {
		return "", fmt.Errorf("xmltv: provider id %q is not a file name", info.Id)
	}
	if strings.TrimSpace(info.Path) == "" {
		return "", errors.New("xmltv: path is required")
	}
	dir := filepath.Join(x.cacheDir, "xmltv")
	file := filepath.Join(dir, info.Id+".xml")
	if stat, err := os.Stat(file); err == nil && !stat.ModTime().Before(x.clock.Now().Add(-XMLTVCacheMaxAge)) {
		return file, nil
	}
	_, err, _ := x.group.Do(file, func() (any, error) {
		return nil, x.refresh(ctx, info, dir, file)
	})
	if err != nil {
		return "", err
	}
	return file, nil
}

func (x *XMLTV) refresh(ctx context.Context, info config.ListingsProviderInfo, dir, file string) error {
	_ = os.Remove(file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("xmltv: %w", err)
	}
	secrets := secret.Collect([]string{info.Path})
	body, source, err := x.source(ctx, info, secrets)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	temp, err := os.CreateTemp(dir, info.Id+".*.tmp")
	if err != nil {
		return fmt.Errorf("xmltv: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = temp.Close()
			_ = os.Remove(temp.Name())
		}
	}()
	var reader io.Reader = body
	// UnzipIfNeededAndCopy decides on the extension of the final URL path
	// (XmlTvListingsProvider.cs:126-128).
	if extension := strings.ToLower(path.Ext(strings.SplitN(source, "?", 2)[0])); extension == ".gz" || extension == ".gzip" {
		unzip, err := gzip.NewReader(body)
		if err != nil {
			return secrets.Err(fmt.Errorf("xmltv: %w", err))
		}
		defer func() { _ = unzip.Close() }()
		reader = unzip
	}
	written, err := io.Copy(temp, contextReader{ctx: ctx, reader: reader})
	if err != nil {
		return secrets.Err(fmt.Errorf("xmltv: %w", err))
	}
	if written == 0 {
		return ErrEmptyXMLTV
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("xmltv: %w", err)
	}
	now := x.clock.Now()
	if err := os.Chtimes(temp.Name(), now, now); err != nil {
		return fmt.Errorf("xmltv: %w", err)
	}
	if err := os.Rename(temp.Name(), file); err != nil {
		return fmt.Errorf("xmltv: %w", err)
	}
	done = true
	return nil
}

// source opens the provider path: a local file when Path does not start
// with "http", else a polite download. It returns the final location for
// the gzip decision.
func (x *XMLTV) source(ctx context.Context, info config.ListingsProviderInfo, secrets secret.Set) (io.ReadCloser, string, error) {
	if !hasPrefixFold(info.Path, "http") {
		file, err := os.Open(info.Path)
		if err != nil {
			return nil, "", secrets.Err(fmt.Errorf("xmltv: %w", err))
		}
		return file, info.Path, nil
	}
	response, err := x.fetch.Get(ctx, fetch.Request{
		Key:     sourceKey(info),
		URL:     info.Path,
		Secrets: secrets,
	})
	if err != nil {
		return nil, "", fmt.Errorf("xmltv: %w", err)
	}
	final := info.Path
	if response.Request != nil && response.Request.URL != nil {
		final = response.Request.URL.String()
	}
	return response.Body, final, nil
}

// sourceKey names the fetch state of a provider. An edit of the path starts
// a new state.
func sourceKey(info config.ListingsProviderInfo) string {
	return "xmltv\x00" + info.Id + "\x00" + info.Path
}

// Forget clears the fetch state of a provider, after the provider is saved
// or deleted. A save is how an administrator retries a rejected source.
func (x *XMLTV) Forget(info config.ListingsProviderInfo) {
	x.fetch.Reset(sourceKey(info))
}

// contextReader stops a copy when ctx ends.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// MapProgramme ports XmlTvListingsProvider.GetProgramInfoWithEtag
// (XmlTvListingsProvider.cs:179-279). A programme whose etag cannot be made
// keeps an empty Etag, so the refresh treats it as changed. A provider with
// DescriptionEpisodes first reads the episode from the text (D-123).
func MapProgramme(p XMLTVProgramme, info config.ListingsProviderInfo) Program {
	if info.DescriptionEpisodes() {
		p = readDescriptionEpisode(p)
	}
	genres := make([]string, 0, len(p.Categories))
	for _, category := range p.Categories {
		if strings.TrimSpace(category) != "" {
			genres = append(genres, category)
		}
	}
	program := Program{
		ChannelID:        p.ChannelID,
		StartDate:        p.StartDate.UTC(),
		EndDate:          p.EndDate.UTC(),
		EpisodeNumber:    p.Episode,
		EpisodeTitle:     p.EpisodeTitle,
		Genres:           genres,
		Name:             p.Title,
		Overview:         p.Description,
		SeasonNumber:     p.Series,
		IsSeries:         p.Episode != nil,
		IsRepeat:         p.IsPreviouslyShown && !p.IsNew,
		IsPremiere:       p.Premiere,
		IsLive:           p.IsLive,
		IsKids:           anyFold(genres, info.KidsCategories),
		IsMovie:          anyFold(genres, info.MovieCategories),
		IsNews:           anyFold(genres, info.NewsCategories),
		IsSports:         anyFold(genres, info.SportsCategories),
		ImageURL:         nonEmpty(p.FirstIcon),
		BackdropImageURL: nonEmpty(p.BackdropPath),
		ThumbImageURL:    nonEmpty(p.StillPath),
		OfficialRating:   nonEmpty(p.FirstRating),
	}
	program.HasImage = program.ImageURL != nil
	if p.CopyrightDate != nil {
		year := p.CopyrightDate.Year()
		program.ProductionYear = &year
	}
	if p.Episode != nil && p.Title != nil {
		seriesID := tuner.DotNetMD5(*p.Title)
		program.SeriesID = &seriesID
	}

	if strings.TrimSpace(p.ProgramID) == "" {
		// Upstream assigns the season and episode suffixes instead of
		// appending them (XmlTvListingsProvider.cs:241-250), so every
		// numbered programme of every show gets md5("-<episode>"). The
		// port appends them (D-74).
		unique := deref(p.Title) + deref(p.EpisodeTitle)
		if program.SeasonNumber != nil {
			unique += "-" + strconv.Itoa(*program.SeasonNumber)
		}
		if program.EpisodeNumber != nil {
			unique += "-" + strconv.Itoa(*program.EpisodeNumber)
		}
		showID := tuner.DotNetMD5(unique)
		if program.IsSeries && !program.IsRepeat && (program.EpisodeNumber == nil || *program.EpisodeNumber == 0) {
			showID += strconv.FormatInt(dotNetTicks(program.StartDate), 10)
		}
		program.ShowID = &showID
	} else {
		showID := p.ProgramID
		program.ShowID = &showID
	}

	program.ID = p.ChannelID + "_" + p.StartDate.Format("2006-01-02T15:04:05.0000000-07:00")

	if program.IsMovie {
		program.IsSeries = false
		program.EpisodeNumber = nil
		program.EpisodeTitle = nil
	}
	if etag, err := Etag(program); err == nil {
		program.Etag = etag
	}
	return program
}

// dotNetTicks is DateTime.Ticks of a UTC time: 100 ns units since 0001-01-01.
func dotNetTicks(t time.Time) int64 {
	const unixEpochTicks = 621355968000000000
	return t.Unix()*10_000_000 + int64(t.Nanosecond()/100) + unixEpochTicks
}

func anyFold(values, set []string) bool {
	for _, value := range values {
		for _, candidate := range set {
			if strings.EqualFold(value, candidate) {
				return true
			}
		}
	}
	return false
}

func nonEmpty(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	return value
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func hasPrefixFold(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}
