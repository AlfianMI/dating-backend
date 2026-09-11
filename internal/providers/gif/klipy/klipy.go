package klipy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/pipigendut/dating-backend/internal/providers/gif"
)

type KlipyProvider struct {
	apiKey     string
	baseURL    string
	showAds    bool
	httpClient *http.Client
}

func NewKlipyProvider(apiKey string, showAds bool) *KlipyProvider {
	return &KlipyProvider{
		apiKey:  apiKey,
		baseURL: "https://api.klipy.com/api/v1",
		showAds: showAds,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (k *KlipyProvider) Name() string {
	return "klipy"
}

// KlipyResponse defines the structure of Klipy API response
type KlipyResponse struct {
	Result bool `json:"result"`
	Data   struct {
		Data []struct {
			ID    string `json:"id"`
			Files struct {
				Gif struct {
					URL    string `json:"url"`
					Width  int    `json:"width"`
					Height int    `json:"height"`
				} `json:"gif"`
				Preview struct {
					URL string `json:"url"`
				} `json:"preview"`
			} `json:"media"` // Note: Klipy often uses "media" or "files"
		} `json:"data"`
	} `json:"data"`
}

// Map Klipy specific structures since actual schema varies slightly.
type KlipyResultItem map[string]interface{}

func (k *KlipyProvider) Search(ctx context.Context, userID, query, locale string, limit, offset int) ([]gif.Gif, error) {
	if limit <= 0 {
		limit = 20
	}
	page := (offset / limit) + 1

	reqURL := fmt.Sprintf("%s/%s/gifs/search?q=%s&page=%d&per_page=%d&customer_id=%s&locale=%s",
		k.baseURL, k.apiKey, url.QueryEscape(query), page, limit, userID, locale)
	return k.fetch(ctx, reqURL)
}

func (k *KlipyProvider) Trending(ctx context.Context, userID, locale string, limit, offset int) ([]gif.Gif, error) {
	if limit <= 0 {
		limit = 20
	}
	page := (offset / limit) + 1

	reqURL := fmt.Sprintf("%s/%s/gifs/trending?page=%d&per_page=%d&customer_id=%s&locale=%s",
		k.baseURL, k.apiKey, page, limit, userID, locale)
	return k.fetch(ctx, reqURL)
}

func (k *KlipyProvider) fetch(ctx context.Context, reqURL string) ([]gif.Gif, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch from klipy: %w", err)
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("failed to close Klipy response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("klipy returned status %d", resp.StatusCode)
	}

	var parsed map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("failed to decode klipy response: %w", err)
	}

	// Try common root keys for results
	var items []interface{}
	if results, ok := parsed["results"].([]interface{}); ok {
		items = results
	} else if data, ok := parsed["data"].([]interface{}); ok {
		items = data
	} else if dataMap, ok := parsed["data"].(map[string]interface{}); ok {
		if actualData, ok := dataMap["data"].([]interface{}); ok {
			items = actualData
		}
	}

	var gifs []gif.Gif
	for _, itemRaw := range items {
		item, ok := itemRaw.(map[string]interface{})
		if !ok {
			continue
		}

		g := gif.Gif{
			Provider: k.Name(),
		}

		if idVal, ok := item["id"]; ok {
			if idStr, ok := idVal.(string); ok {
				g.ID = idStr
			} else if idNum, ok := idVal.(float64); ok {
				g.ID = fmt.Sprintf("%.0f", idNum)
			}
		}

		// Try various media container keys (v1 uses "file")
		var fileObj map[string]interface{}
		for _, key := range []string{"file", "files", "media", "media_formats"} {
			if m, ok := item[key].(map[string]interface{}); ok {
				fileObj = m
				break
			}
		}

		if fileObj != nil {
			// Find main GIF URL from quality tiers: hd -> md -> original
			for _, quality := range []string{"hd", "md", "original"} {
				if qObj, ok := fileObj[quality].(map[string]interface{}); ok {
					// Within quality, try formats: gif -> mp4
					for _, format := range []string{"gif", "mp4"} {
						if fObj, ok := qObj[format].(map[string]interface{}); ok {
							if u, ok := fObj["url"].(string); ok {
								g.URL = u
							}
							if w, ok := fObj["width"].(float64); ok {
								g.Width = int(w)
							}
							if h, ok := fObj["height"].(float64); ok {
								g.Height = int(h)
							}
							if g.URL != "" {
								break
							}
						}
					}
					if g.URL != "" {
						break
					}
				} else if qObj, ok := fileObj["url"].(string); ok {
					// Fallback if the quality key is actually the URL itself (unlikely for v1 but safe)
					g.URL = qObj
					break
				}
			}

			// Find preview URL from quality tiers: sm -> xs -> md
			for _, quality := range []string{"sm", "xs", "md", "preview"} {
				if qObj, ok := fileObj[quality].(map[string]interface{}); ok {
					// For preview, GIF is more universally supported by standard Image components
					for _, format := range []string{"gif", "webp", "jpg"} {
						if fObj, ok := qObj[format].(map[string]interface{}); ok {
							if u, ok := fObj["url"].(string); ok {
								g.Preview = u
								break
							}
						}
					}
					if g.Preview != "" {
						break
					}
				}
			}
		}

		// Fallback for preview
		if g.Preview == "" {
			g.Preview = g.URL
		}

		// Only add if we at least have a URL
		if g.URL != "" {
			gifs = append(gifs, g)
		}
	}

	return gifs, nil
}
