package xiaomi_cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
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

func (d *XiaomiCloud) updateSessionFromResponse(res *resty.Response, expectedServiceToken string) {
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	if d.session.serviceToken != expectedServiceToken {
		return
	}
	d.session.apply(res.Cookies())
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
	log.Debugf("[xiaomi_cloud] 开始获取云盘会话: storage_id=%d", d.ID)
	if err := d.refreshServiceToken(ctx); err != nil {
		return errs.NewErr(errs.EmptyToken, "账号凭据续签云盘会话失败: %v", err)
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
	accountCookies, err := d.accountCookies()
	if err != nil {
		return err
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
		finalURL := serviceResp.RawResponse.Request.URL
		missing := []string{}
		for _, cookie := range newSession.allCookies() {
			if cookie.Value == "" {
				missing = append(missing, cookie.Name)
			}
		}
		// 日志只记录跳转路径和缺失字段，签名、票据及 Cookie 值不得写入日志。
		log.Warnf("[xiaomi_cloud] 云盘会话获取失败: storage_id=%d status=%d host=%s path=%s missing=%s",
			d.ID, serviceResp.StatusCode(), finalURL.Hostname(), finalURL.EscapedPath(), strings.Join(missing, ","))
		return fmt.Errorf("账号登录未签发完整的云盘会话，缺少 %s；最终页面 %s%s（HTTP %d）",
			strings.Join(missing, ", "), finalURL.Hostname(), finalURL.EscapedPath(), serviceResp.StatusCode())
	}
	d.sessionMu.Lock()
	d.session = newSession
	d.sessionMu.Unlock()
	d.updateAccountCredentials(jar.Cookies(accountURL))

	log.Infof("[xiaomi_cloud] serviceToken 已自动续签: storage_id=%d", d.ID)
	return nil
}

func (d *XiaomiCloud) accountCookies() ([]*http.Cookie, error) {
	cookies := []*http.Cookie{
		{Name: "passToken", Value: d.PassToken},
		{Name: "userId", Value: d.UserID},
	}
	for _, cookie := range cookies {
		if strings.TrimSpace(cookie.Value) == "" {
			return nil, fmt.Errorf("%s 不能为空", cookie.Name)
		}
		if err := cookie.Valid(); err != nil {
			return nil, fmt.Errorf("%s 格式无效，请仅填写 Cookie 的值", cookie.Name)
		}
	}
	return cookies, nil
}

func (d *XiaomiCloud) updateAccountCredentials(cookies []*http.Cookie) {
	changed := false
	for _, cookie := range cookies {
		if cookie.Value == "" {
			continue
		}
		switch cookie.Name {
		case "passToken":
			changed = changed || d.PassToken != cookie.Value
			d.PassToken = cookie.Value
		case "userId":
			changed = changed || d.UserID != cookie.Value
			d.UserID = cookie.Value
		}
	}
	// 登录成功后保存轮换的账号凭据，重启时直接使用最新值；其余 Cookie 不写入配置。
	if changed && d.ID != 0 {
		op.MustSaveDriverStorage(d)
		log.Infof("[xiaomi_cloud] 账号凭据发生轮换: storage_id=%d", d.ID)
	}
}

func (s sessionCookies) allCookies() []*http.Cookie {
	return []*http.Cookie{
		{Name: "serviceToken", Value: s.serviceToken},
		{Name: "userId", Value: s.userID},
		{Name: "i.mi.com_slh", Value: s.sessionHash},
		{Name: "i.mi.com_ph", Value: s.userHash},
	}
}

func (s sessionCookies) httpCookies() []*http.Cookie {
	cookies := make([]*http.Cookie, 0, 4)
	for _, cookie := range s.allCookies() {
		if cookie.Value != "" {
			cookies = append(cookies, cookie)
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
