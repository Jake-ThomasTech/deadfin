// SPDX-License-Identifier: GPL-2.0-only

package tmdb

import (
	"context"
	"log/slog"
	"net/url"
	"strconv"
)

// collection is the /collection/{id} response with the appended images.
type collection struct {
	ID           int     `json:"id"`
	Name         string  `json:"name"`
	Overview     string  `json:"overview"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	Images       *images `json:"images"`
}

type searchCollection struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type searchCollectionResponse struct {
	Results []searchCollection `json:"results"`
}

// getCollection ports TmdbClientManager.GetCollectionAsync, which asks
// TMDbLib for the collection with CollectionMethods.Images.
func (client *apiClient) getCollection(ctx context.Context, tmdbID int, language, imageLanguages string) (*collection, error) {
	query := url.Values{}
	if language != "" {
		query.Set("language", language)
	}
	if imageLanguages != "" {
		query.Set("include_image_language", imageLanguages)
	}
	query.Set("append_to_response", "images")
	var result collection
	found, err := client.getJSON(ctx, "/collection/"+strconv.Itoa(tmdbID), query, &result)
	if err != nil {
		client.logger.LogAttrs(ctx, slog.LevelWarn, "tmdb get failed", slog.String("op", "collection"), slog.Int("tmdbId", tmdbID), slog.String("error", "request failed"))
		return nil, err
	}
	client.logger.LogAttrs(ctx, slog.LevelDebug, "tmdb get", slog.String("op", "collection"), slog.Int("tmdbId", tmdbID), slog.Bool("found", found))
	if !found {
		return nil, nil
	}
	return &result, nil
}

// searchCollection ports TmdbClientManager.SearchCollectionAsync.
func (client *apiClient) searchCollection(ctx context.Context, name, language string) ([]searchCollection, error) {
	query := url.Values{}
	query.Set("query", name)
	if language != "" {
		query.Set("language", language)
	}
	var response searchCollectionResponse
	if _, err := client.getJSON(ctx, "/search/collection", query, &response); err != nil {
		client.logger.LogAttrs(ctx, slog.LevelWarn, "tmdb search failed", slog.String("op", "search/collection"), slog.String("error", "request failed"))
		return nil, err
	}
	client.logger.LogAttrs(ctx, slog.LevelDebug, "tmdb search", slog.String("op", "search/collection"), slog.Int("results", len(response.Results)))
	return response.Results, nil
}
