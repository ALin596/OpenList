package xiaomi_cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/avast/retry-go"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

const (
	BaseURL          = "https://i.mi.com"
	AccountBaseURL   = "https://account.xiaomi.com"
	APIPrefix        = "/drive/v2/user"
	UserAgent        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	Referer          = "https://i.mi.com/drive/h5"
	maxAuthRedirects = 10
)

type requestCallback func(req *resty.Request, serviceToken string)

type httpStatusError struct {
	code   int
	status string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP错误: %d %s", e.code, e.status)
}

type apiError struct {
	code        int
	description string
	retriable   bool
}

func (e *apiError) Error() string {
	return fmt.Sprintf("API错误: code=%d, msg=%s", e.code, e.description)
}

func (d *XiaomiCloud) request(ctx context.Context, rawURL, method string, callback requestCallback, resp interface{}) ([]byte, error) {
	if isXiaomiAPIURL(rawURL) && d.sessionSnapshot().serviceToken == "" {
		if err := d.ensureServiceToken(ctx, ""); err != nil {
			return nil, err
		}
	}

	res, usedSession, err := d.doRequest(ctx, rawURL, method, callback)
	if err != nil {
		return nil, err
	}
	if res.StatusCode() == http.StatusUnauthorized && isXiaomiAPIURL(rawURL) {
		if err = d.ensureServiceToken(ctx, usedSession.serviceToken); err != nil {
			return nil, err
		}
		res, _, err = d.doRequest(ctx, rawURL, method, callback)
		if err != nil {
			return nil, err
		}
	}

	if res.StatusCode() != http.StatusOK {
		if res.StatusCode() == http.StatusUnauthorized {
			return nil, errs.NewErr(errs.EmptyToken, "account_cookie 无法续签有效的 serviceToken")
		}
		return nil, &httpStatusError{code: res.StatusCode(), status: res.Status()}
	}

	d.updateSessionFromResponse(res)
	body := res.Body()
	if resp != nil {
		if err = json.Unmarshal(body, resp); err != nil {
			return body, fmt.Errorf("解析响应失败: %w", err)
		}
		var errResp ErrorResp
		if err = json.Unmarshal(body, &errResp); err == nil && errResp.Code != 0 && errResp.Result != "ok" {
			return body, &apiError{
				code:        errResp.Code,
				description: errResp.Description,
				retriable:   errResp.Retriable,
			}
		}
	}

	log.Debugf("[xiaomi_cloud] 请求成功: %s %s", method, rawURL)
	return body, nil
}

func (d *XiaomiCloud) doRequest(ctx context.Context, rawURL, method string, callback requestCallback) (*resty.Response, sessionCookies, error) {
	req := base.RestyClient.R().SetContext(ctx)
	session := d.sessionSnapshot()
	req.SetHeaders(map[string]string{
		"User-Agent":      UserAgent,
		"Referer":         Referer,
		"Accept":          "*/*",
		"Accept-Language": "zh-CN,zh;q=0.9",
	})
	if isXiaomiAPIURL(rawURL) {
		cookies := session.httpCookies()
		cookies = append(cookies,
			&http.Cookie{Name: "uLocale", Value: "zh_CN"},
			&http.Cookie{Name: "iplocale", Value: "zh_CN"},
		)
		req.SetCookies(cookies)
	}
	req.SetQueryParam("ts", fmt.Sprint(time.Now().UnixMilli()))
	if callback != nil {
		callback(req, session.serviceToken)
	}
	res, err := req.Execute(method, rawURL)
	return res, session, err
}

func buildFormData(data map[string]string) string {
	values := url.Values{}
	for key, value := range data {
		values.Set(key, value)
	}
	return values.Encode()
}

func isXiaomiAPIURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(parsed.Hostname(), "i.mi.com")
}

func retryRequest(ctx context.Context, attempts int, fn func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	return retry.Do(fn,
		retry.Context(ctx),
		retry.Attempts(uint(attempts)),
		retry.Delay(time.Second),
		retry.DelayType(retry.BackOffDelay),
		retry.RetryIf(isRetriableError),
		retry.LastErrorOnly(true),
		retry.OnRetry(func(attempt uint, err error) {
			log.Warnf("[xiaomi_cloud] 请求失败,准备第 %d 次重试: %v", attempt+1, err)
		}),
	)
}

func isRetriableError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		return statusErr.code == http.StatusRequestTimeout ||
			statusErr.code == http.StatusTooManyRequests ||
			statusErr.code >= http.StatusInternalServerError
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.retriable
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && (networkErr.Timeout() || networkErr.Temporary()) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	errText := strings.ToLower(err.Error())
	return strings.Contains(errText, "http错误: 502") ||
		strings.Contains(errText, "http错误: 503") ||
		strings.Contains(errText, "http错误: 504") ||
		strings.Contains(errText, "connection reset")
}

func normalizeError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch apiErr.code {
		case http.StatusUnauthorized:
			return errs.NewErr(errs.EmptyToken, "account_cookie 已失效或需要重新提取")
		case http.StatusForbidden:
			return fmt.Errorf("访问被拒绝: %w", err)
		case http.StatusNotFound:
			return errs.NewErr(errs.ObjectNotFound, "文件或文件夹不存在")
		}
	}
	errMsg := err.Error()
	if strings.Contains(errMsg, "code=401") || strings.Contains(errMsg, "未授权") {
		return errs.NewErr(errs.EmptyToken, "account_cookie 已失效或需要重新提取")
	}
	if strings.Contains(errMsg, "code=403") || strings.Contains(errMsg, "禁止访问") {
		return fmt.Errorf("访问被拒绝: %w", err)
	}
	if strings.Contains(errMsg, "code=404") || strings.Contains(errMsg, "不存在") {
		return errs.NewErr(errs.ObjectNotFound, "文件或文件夹不存在")
	}
	return err
}
