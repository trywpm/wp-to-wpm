package svn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/net/html"
)

var (
	httpClient = &http.Client{}
)

var (
	hrefBytes   = []byte("href")
	parentBytes = []byte("../")
)

func List(ctx context.Context, svnRepo string, isValid func([]byte) bool) (map[string]struct{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svnRepo, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch data from %s: %w", svnRepo, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch repo %s, status code: %d", svnRepo, resp.StatusCode)
	}

	list := make(map[string]struct{})
	z := html.NewTokenizer(resp.Body)

	for {
		switch z.Next() {
		case html.ErrorToken:
			err := z.Err()
			if errors.Is(err, io.EOF) {
				return list, nil
			}
			return nil, fmt.Errorf("error tokenizing html from %s: %w", svnRepo, err)

		case html.StartTagToken:
			name, hasAttr := z.TagName()
			if !hasAttr || len(name) != 1 || name[0] != 'a' {
				continue
			}

			for {
				k, v, more := z.TagAttr()
				if bytes.Equal(k, hrefBytes) {
					if len(v) > 1 && v[len(v)-1] == '/' && !bytes.Equal(v, parentBytes) {
						slug := v[:len(v)-1]

						if isValid != nil && !isValid(slug) {
							continue
						}

						list[string(slug)] = struct{}{}
					}
					break
				}
				if !more {
					break
				}
			}
		}
	}
}
