package xiaomi_cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/publicsuffix"
)

type sessionCookies struct {
	serviceToken string
	userID       string
	sessionHash  string
	userHash     string
}

var newXiaomiAuthClient = base.NewRestyClient

func (d *XiaomiCloud) sessionSnapshot() sessionCookies {
	d.sessionMu.RLock()
	defer d.sessionMu.RUnlock()
	return d.session
}

func (d *XiaomiCloud) updateSessionFromResponse(res *resty.Response) {
	d.sessionMu.Lock()
	d.session.apply(res.Cookies())
	d.sessionMu.Unlock()
}

func (d *XiaomiCloud) ensureServiceToken(ctx context.Context, staleToken string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	currentToken := d.sessionSnapshot().serviceToken
	if currentToken != "" && (staleToken == "" || currentToken != staleToken) {
		return nil
	}
	if err := d.refreshServiceToken(ctx); err != nil {
		return errs.NewErr(errs.EmptyToken, "account_cookie 已失效或需要完成小米账号安全验证: %v", err)
	}
	return nil
}

func (d *XiaomiCloud) refreshServiceToken(ctx context.Context) error {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return fmt.Errorf("创建 Cookie Jar 失败: %w", err)
	}

	accountURL, _ := url.Parse(accountBaseURL + "/pass/serviceLogin")
	cloudURL, _ := url.Parse(baseURL + "/")
	accountCookies := parseCookieHeader(d.AccountCookie)
	if !containsCookie(accountCookies, "passToken") {
		return fmt.Errorf("account_cookie 中缺少 passToken")
	}
	jar.SetCookies(accountURL, accountCookies)
	jar.SetCookies(cloudURL, d.sessionSnapshot().httpCookies())

	client := newXiaomiAuthClient().SetCookieJar(jar).SetRedirectPolicy(
		resty.RedirectPolicyFunc(func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxAuthRedirects {
				return fmt.Errorf("登录跳转次数超过限制")
			}
			switch strings.ToLower(req.URL.Hostname()) {
			case "account.xiaomi.com", "i.mi.com":
				return nil
			default:
				return fmt.Errorf("拒绝登录跳转到非小米域名")
			}
		}),
	)

	loginResp, err := client.R().
		SetContext(ctx).
		SetHeaders(map[string]string{
			"User-Agent":      userAgent,
			"Referer":         referer,
			"Accept":          "application/json, text/plain, */*",
			"Accept-Language": "zh-CN,zh;q=0.9",
		}).
		SetQueryParams(map[string]string{
			"ts":       strconv.FormatInt(time.Now().UnixMilli(), 10),
			"followUp": referer,
			"_locale":  "zh_CN",
		}).
		Get(baseURL + "/api/user/login")
	if err != nil {
		return fmt.Errorf("获取登录地址失败: %w", err)
	}
	if loginResp.StatusCode() != http.StatusOK {
		return fmt.Errorf("获取登录地址失败: HTTP %d", loginResp.StatusCode())
	}

	var loginResult struct {
		Code int `json:"code"`
		Data struct {
			LoginURL string `json:"loginUrl"`
		} `json:"data"`
	}
	if err = json.Unmarshal(loginResp.Body(), &loginResult); err != nil {
		return fmt.Errorf("解析登录地址失败: %w", err)
	}
	if loginResult.Code != 0 || loginResult.Data.LoginURL == "" {
		return fmt.Errorf("小米云盘未返回登录地址")
	}

	serviceLoginURL, err := url.Parse(loginResult.Data.LoginURL)
	if err != nil || serviceLoginURL.Scheme != "https" || !strings.EqualFold(serviceLoginURL.Hostname(), "account.xiaomi.com") {
		return fmt.Errorf("小米云盘返回了无效的登录地址")
	}
	serviceResp, err := client.R().
		SetContext(ctx).
		SetHeaders(map[string]string{
			"User-Agent":      userAgent,
			"Referer":         referer,
			"Accept-Language": "zh-CN,zh;q=0.9",
		}).
		Get(serviceLoginURL.String())
	if err != nil {
		return fmt.Errorf("执行小米账号登录失败: %w", err)
	}
	if serviceResp.StatusCode() >= http.StatusBadRequest {
		return fmt.Errorf("执行小米账号登录失败: HTTP %d", serviceResp.StatusCode())
	}

	newSession := sessionCookiesFromHTTP(jar.Cookies(cloudURL))
	if !newSession.complete() {
		return fmt.Errorf("账号登录未签发完整的云盘会话")
	}
	newAccountCookie := cookieHeader(jar.Cookies(accountURL))
	d.sessionMu.Lock()
	accountCookieChanged := newAccountCookie != "" && newAccountCookie != d.AccountCookie
	if newAccountCookie != "" {
		d.AccountCookie = newAccountCookie
	}
	d.session = newSession
	d.sessionMu.Unlock()

	if accountCookieChanged && d.ID != 0 {
		op.MustSaveDriverStorage(d)
	}
	log.Infof("[xiaomi_cloud] serviceToken 已自动续签")
	return nil
}

func (s sessionCookies) httpCookies() []*http.Cookie {
	values := map[string]string{
		"serviceToken": s.serviceToken,
		"userId":       s.userID,
		"i.mi.com_slh": s.sessionHash,
		"i.mi.com_ph":  s.userHash,
	}
	cookies := make([]*http.Cookie, 0, len(values))
	for _, name := range []string{"serviceToken", "userId", "i.mi.com_slh", "i.mi.com_ph"} {
		if values[name] != "" {
			cookies = append(cookies, &http.Cookie{Name: name, Value: values[name]})
		}
	}
	return cookies
}

func (s *sessionCookies) apply(cookies []*http.Cookie) {
	for _, cookie := range cookies {
		if cookie.Value == "" {
			continue
		}
		switch cookie.Name {
		case "serviceToken":
			s.serviceToken = cookie.Value
		case "userId":
			s.userID = cookie.Value
		case "i.mi.com_slh":
			s.sessionHash = cookie.Value
		case "i.mi.com_ph":
			s.userHash = cookie.Value
		}
	}
}

func (s sessionCookies) complete() bool {
	return s.serviceToken != "" && s.userID != "" && s.sessionHash != "" && s.userHash != ""
}

func sessionCookiesFromHTTP(cookies []*http.Cookie) sessionCookies {
	var session sessionCookies
	session.apply(cookies)
	return session
}

func parseCookieHeader(cookieString string) []*http.Cookie {
	req := &http.Request{Header: make(http.Header)}
	req.Header.Set("Cookie", cookieString)
	return req.Cookies()
}

func containsCookie(cookies []*http.Cookie, name string) bool {
	for _, cookie := range cookies {
		if cookie.Name == name && cookie.Value != "" {
			return true
		}
	}
	return false
}

func hasCookie(cookieString, name string) bool {
	return containsCookie(parseCookieHeader(cookieString), name)
}

func cookieHeader(cookies []*http.Cookie) string {
	values := make(map[string]string, len(cookies))
	for _, cookie := range cookies {
		if cookie.Name != "" && cookie.Value != "" {
			values[cookie.Name] = cookie.Value
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, (&http.Cookie{Name: name, Value: values[name]}).String())
	}
	return strings.Join(pairs, "; ")
}
