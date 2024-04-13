package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/kondohiroki/go-grpc-boilerplate/internal/logger"
	"go.uber.org/zap"
)

var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

type HttpRequest struct {
	HttpClient *http.Client
	Url        string
	Method     string
	Headers    map[string]string
	Body       []byte
	Query      map[string]string
	Params     map[string]string

	onRenewBearer func(context.Context) (string, error)
}

// WithBearer sets bearer token in authorization header. The renewerFunc can be provided
// if you want to renew a token when got 401 response where the returned string is a newly token.
func (req *HttpRequest) WithBearer(token string, renewerFunc ...func(context.Context) (string, error)) {
	if req.Headers == nil {
		req.Headers = make(map[string]string)
	}
	req.Headers["Authorization"] = "Bearer " + token

	if len(renewerFunc) > 0 {
		req.onRenewBearer = renewerFunc[0]
	}
}

func NewHTTPClient() *http.Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Timeout:   time.Second * 300,
		Transport: transport,
	}
}

func MakeHTTPRequest(ctx context.Context, req HttpRequest) (*http.Response, error) {
	httpClient := req.HttpClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if httpClient.Timeout == 0 {
		httpClient.Timeout = 30 * time.Second
	}

	buf := bufferPool.Get().(*bytes.Buffer)
	defer bufferPool.Put(buf)
	buf.Reset()
	buf.ReadFrom(bytes.NewReader(req.Body))

	for {
		httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.Url, buf)
		if err != nil {
			return nil, err
		}

		for key, value := range req.Headers {
			httpReq.Header.Add(key, value)
			// TODO: Add request-id for every outgoing requests
		}

		if req.Query != nil {
			query := httpReq.URL.Query()
			for key, value := range req.Query {
				query.Add(key, value)
			}
			httpReq.URL.RawQuery = query.Encode()
		}

		if req.Params != nil {
			path := httpReq.URL.Path
			for key, value := range req.Params {
				path = strings.Replace(path, ":"+key, value, -1)
			}
			httpReq.URL.Path = path
		}

		logger.Log.Debug(fmt.Sprintf("making HTTP request to %s headers: %v", req.Url, req.Headers))
		logger.Log.Debug(fmt.Sprintf("request body to %s is %s", req.Url, buf))

		res, err := httpClient.Do(httpReq)
		if err != nil {
			return nil, err
		}

		if res.StatusCode == http.StatusTemporaryRedirect {
			location := res.Header.Get("Location")
			if location == "" {
				return res, errors.New("no Location header found in 307 response")
			}
			req.Url = location
			continue
		}

		if res.StatusCode == http.StatusUnauthorized && req.onRenewBearer != nil {
			logger.Log.Debug("got 401 response, token renewer function is provided, renewing a token..")
			token, err := req.onRenewBearer(ctx)
			if err != nil {
				return nil, err
			}
			req.Headers["Authorization"] = "Bearer " + token
			buf.Reset()
			buf.ReadFrom(bytes.NewReader(req.Body))
			continue
		}

		logger.Log.Debug(fmt.Sprintf("HTTP response status from %s is %s", req.Url, res.Status))
		return res, nil
	}
}

// The function makes an HTTP request and automatically parses the response body into either a success
// or error object based on the HTTP status code.
// if it has error response, then you can handle it with errorBody
func RequestAutoBodyParser(ctx context.Context, req HttpRequest, result interface{}) (*http.Response, []byte, error) {
	resp, err := MakeHTTPRequest(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	defer resp.Body.Close()

	maxBodySize := 10000 // 2kb
	if len(body) > maxBodySize {
		logger.Log.Debug(fmt.Sprintf("response body from %s", req.Url), zap.String("body", "...(too large to print)"), zap.Int("status_code", resp.StatusCode))
	} else {
		logger.Log.Debug(fmt.Sprintf("response body from %s", req.Url), zap.ByteString("body", body), zap.Int("status_code", resp.StatusCode))
	}

	// logger.Log.Debug(fmt.Sprintf("response body from %s", req.Url), zap.ByteString("body", body), zap.Int("status_code", resp.StatusCode))

	// Http status code 4xx,5xx is considered as error
	if resp.StatusCode >= 500 {
		return resp, body, fmt.Errorf("(Unxpected 5xx) got %d response from %s is %s", resp.StatusCode, req.Url, body)
	}

	// Accept 1xx, 2xx, 3xx to be parsed and will be handled by the caller
	if err := sonic.Unmarshal(body, result); err != nil {
		return resp, body, fmt.Errorf("(Unxpected 4xx) got %d response from %s is %s", resp.StatusCode, req.Url, body)
	}

	return resp, body, nil
}
