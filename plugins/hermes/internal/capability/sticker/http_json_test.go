package sticker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func apiHzConfig() HTTPJSONConfig {
	return HTTPJSONConfig{
		ProviderID: "apihz",
		Endpoint:   "https://cn.apihz.cn/api/img/apihzbqb.php",
		Method:     http.MethodGet,
		Parameters: map[string]string{
			"id":    "${env:APIHZ_ID}",
			"key":   "${env:APIHZ_KEY}",
			"type":  "2",
			"limit": "${limit}",
			"words": "${query}",
			"page":  "${page}",
		},
		SuccessPath:       "code",
		SuccessValues:     []string{"200"},
		ItemsPath:         "res",
		URLPath:           "$",
		ErrorPath:         "msg",
		URLTransforms:     []string{TransformTrim, TransformMarkdownLinkTarget},
		MediaAllowedHosts: []string{"img.example.com"},
		RequestsPerMinute: 60,
	}
}

func TestHTTPJSONProviderAPIHzMappingAndGETTemplates(t *testing.T) {
	var captured *http.Request
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		captured = request.Clone(request.Context())
		return jsonResponse(http.StatusOK, `{
            "code": 200,
            "res": [
              "[https://img.example.com/a.gif](https://img.example.com/a.gif)",
              " https://img.example.com/b.png "
            ]
        }`), nil
	})}
	provider, err := NewHTTPJSONProvider(
		apiHzConfig(),
		WithProviderHTTPClient(client),
		WithProviderEnvironment(func(name string) (string, bool) {
			values := map[string]string{"APIHZ_ID": "account-id", "APIHZ_KEY": "secret-key"}
			value, exists := values[name]
			return value, exists
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Search(context.Background(), ProviderSearchRequest{
		Query: "猫 猫",
		Limit: 10,
		Page:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("got %d candidates, want 2", len(result))
	}
	if result[0].Reference != "https://img.example.com/a.gif" || result[1].Reference != "https://img.example.com/b.png" {
		t.Fatalf("unexpected mapping: %#v", result)
	}
	if captured == nil {
		t.Fatal("request was not captured")
	}
	query := captured.URL.Query()
	for key, expected := range map[string]string{
		"id": "account-id", "key": "secret-key", "type": "2",
		"limit": "10", "words": "猫 猫", "page": "2",
	} {
		if query.Get(key) != expected {
			t.Errorf("query %s=%q, want %q", key, query.Get(key), expected)
		}
	}
}

func TestHTTPJSONProviderPOSTFormAndObjectArray(t *testing.T) {
	var form url.Values
	var version string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost {
			t.Fatalf("method=%s", request.Method)
		}
		if request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("content-type=%q", request.Header.Get("Content-Type"))
		}
		version = request.URL.Query().Get("version")
		data, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		form, readErr = url.ParseQuery(string(data))
		if readErr != nil {
			t.Fatal(readErr)
		}
		return jsonResponse(http.StatusOK, `{
          "ok": true,
          "data": [
            {"source":{"url":" https://cdn.example/a.webp "},"title":"A &amp; B"},
            {"source":{"url":"https://cdn.example/b.jpg"},"title":"second"}
          ]
        }`), nil
	})}
	provider, err := NewHTTPJSONProvider(HTTPJSONConfig{
		ProviderID:            "objects",
		Endpoint:              "https://api.example/search",
		Method:                http.MethodPost,
		Query:                 map[string]string{"version": "v1"},
		Form:                  map[string]string{"q": "${query}", "limit": "${limit}", "page": "${page}"},
		SuccessPath:           "ok",
		SuccessValues:         []string{"true"},
		ItemsPath:             "data",
		URLPath:               "source.url",
		DescriptionPath:       "title",
		URLTransforms:         []string{TransformTrim},
		DescriptionTransforms: []string{TransformHTMLUnescape, TransformTrim},
		MediaAllowedHosts:     []string{"cdn.example"},
	}, WithProviderHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Search(context.Background(), ProviderSearchRequest{Query: "hello", Limit: 3, Page: 4})
	if err != nil {
		t.Fatal(err)
	}
	if form.Get("q") != "hello" || form.Get("limit") != "3" || form.Get("page") != "4" {
		t.Fatalf("unexpected form: %v", form)
	}
	if version != "v1" {
		t.Fatalf("query version=%q", version)
	}
	if len(result) != 2 || result[0].Description != "A & B" || result[0].Reference != "https://cdn.example/a.webp" {
		t.Fatalf("unexpected mapped objects: %#v", result)
	}
}

type recordingDownloader struct {
	url    string
	policy DownloadPolicy
}

func (d *recordingDownloader) Download(_ context.Context, rawURL string, policy DownloadPolicy) ([]byte, string, error) {
	d.url = rawURL
	d.policy = policy
	return []byte("GIF89a"), "image/gif", nil
}

func TestHTTPJSONProviderMaterializeReturnsSelfContainedEmoji(t *testing.T) {
	downloader := new(recordingDownloader)
	config := apiHzConfig()
	config.MaxMediaBytes = 12345
	provider, err := NewHTTPJSONProvider(config, WithProviderDownloader(downloader))
	if err != nil {
		t.Fatal(err)
	}
	output, err := provider.Materialize(context.Background(), ProviderCandidate{
		Reference: "https://img.example.com/a.gif", Description: "开心",
	})
	if err != nil {
		t.Fatal(err)
	}
	if downloader.url != "https://img.example.com/a.gif" || downloader.policy.MaxBytes != 12345 {
		t.Fatalf("download=%q policy=%#v", downloader.url, downloader.policy)
	}
	if len(downloader.policy.AllowedHosts) != 1 || downloader.policy.AllowedHosts[0] != "img.example.com" {
		t.Fatalf("allowed hosts=%v", downloader.policy.AllowedHosts)
	}
	if string(output.Data) != "GIF89a" || output.URL != "" || output.MD5 == "" || output.Description != "开心" {
		t.Fatalf("unexpected emoji output: %#v", output)
	}
}

func TestHTTPJSONProviderScalarItemAndBusinessError(t *testing.T) {
	t.Run("scalar", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":"ok","result":"https://img.example.com/one.gif"}`), nil
		})}
		config := apiHzConfig()
		config.SuccessValues = []string{"ok"}
		config.ItemsPath = "result"
		config.Parameters = nil
		provider, err := NewHTTPJSONProvider(config, WithProviderHTTPClient(client))
		if err != nil {
			t.Fatal(err)
		}
		result, err := provider.Search(context.Background(), ProviderSearchRequest{Query: "one", Limit: 1, Page: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(result) != 1 || result[0].Reference != "https://img.example.com/one.gif" {
			t.Fatalf("unexpected scalar result: %#v", result)
		}
	})

	t.Run("business error", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":403,"msg":"quota exhausted","res":[]}`), nil
		})}
		config := apiHzConfig()
		config.Parameters = nil
		provider, err := NewHTTPJSONProvider(config, WithProviderHTTPClient(client))
		if err != nil {
			t.Fatal(err)
		}
		_, err = provider.Search(context.Background(), ProviderSearchRequest{Query: "one", Limit: 1, Page: 1})
		var business *BusinessError
		if !errors.As(err, &business) {
			t.Fatalf("error=%v, want BusinessError", err)
		}
		if business.Code != "403" || business.Message != "quota exhausted" {
			t.Fatalf("unexpected business error: %#v", business)
		}
	})
}

func TestHTTPJSONProviderCredentialsNeverAppearInTransportError(t *testing.T) {
	const secret = "must-not-leak"
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("failed URL " + request.URL.String())
	})}
	config := apiHzConfig()
	provider, err := NewHTTPJSONProvider(config,
		WithProviderHTTPClient(client),
		WithProviderEnvironment(func(string) (string, bool) { return secret, true }),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Search(context.Background(), ProviderSearchRequest{Query: "q", Limit: 1, Page: 1})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("credential leaked in error: %v", err)
	}
}

func TestHTTPJSONProviderRedactsCredentialsReflectedInDescription(t *testing.T) {
	const secret = "must-not-reach-agent"
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"code":200,"res":[{"url":"https://img.example.com/a.png","description":"token must-not-reach-agent"}]}`), nil
	})}
	config := apiHzConfig()
	config.Parameters = map[string]string{"key": "${env:KEY}"}
	config.URLPath = "url"
	config.DescriptionPath = "description"
	provider, err := NewHTTPJSONProvider(config,
		WithProviderHTTPClient(client),
		WithProviderEnvironment(func(string) (string, bool) { return secret, true }),
	)
	if err != nil {
		t.Fatal(err)
	}
	values, err := provider.Search(context.Background(), ProviderSearchRequest{Query: "q", Limit: 1, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || strings.Contains(values[0].Description, secret) || !strings.Contains(values[0].Description, "***") {
		t.Fatalf("description was not redacted: %#v", values)
	}
}

func TestHTTPJSONProviderDisablesRedirectsEvenWithInjectedClient(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			requests := 0
			const secret = "redirect-secret"
			client := &http.Client{
				// This permissive policy must be overridden by the provider.
				CheckRedirect: func(*http.Request, []*http.Request) error { return nil },
				Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					requests++
					if requests > 1 {
						t.Fatalf("provider followed redirect and could leak credentials to %s", request.URL.Host)
					}
					response := jsonResponse(status, "")
					response.Header.Set("Location", "https://evil.example/collect")
					response.Request = request
					return response, nil
				}),
			}
			config := apiHzConfig()
			config.Method = http.MethodPost
			config.Parameters = map[string]string{"key": "${env:KEY}", "q": "${query}"}
			config.Headers = map[string]string{"Authorization": "Bearer ${env:KEY}"}
			provider, err := NewHTTPJSONProvider(config,
				WithProviderHTTPClient(client),
				WithProviderEnvironment(func(string) (string, bool) { return secret, true }),
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Search(context.Background(), ProviderSearchRequest{Query: "cat", Limit: 1, Page: 1})
			if err == nil {
				t.Fatal("expected redirect failure")
			}
			if requests != 1 {
				t.Fatalf("requests=%d, want 1", requests)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("credential leaked in redirect error: %v", err)
			}
		})
	}
}

func TestTemplateExpansionAndTransforms(t *testing.T) {
	expanded, secrets, err := expandTemplate(
		"q=${query}&n=${limit}&p=${page}&key=${env:KEY}",
		templateValues{
			query: "猫", limit: 9, page: 3,
			lookupEnv: func(name string) (string, bool) { return "token", name == "KEY" },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if expanded != "q=猫&n=9&p=3&key=token" || len(secrets) != 1 || secrets[0] != "token" {
		t.Fatalf("expanded=%q secrets=%v", expanded, secrets)
	}
	value, err := applyTransforms(" [label](https://example/a.gif) ", []string{TransformTrim, TransformMarkdownLinkTarget})
	if err != nil || value != "https://example/a.gif" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	value, err = applyTransforms("A &amp; B", []string{TransformHTMLUnescape})
	if err != nil || value != "A & B" {
		t.Fatalf("value=%q err=%v", value, err)
	}
}
