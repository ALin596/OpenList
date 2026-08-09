package xiaomi_cloud

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	AccountCookie string `json:"account_cookie" required:"true" type:"text" help:"account.xiaomi.com 请求中的完整 Cookie，必须包含 passToken"`

	// 可选配置
	OrderBy        string `json:"order_by" type:"select" options:"SERVICE_TIME,NAME,SIZE,CREATE_TIME,MODIFY_TIME" default:"SERVICE_TIME" help:"文件排序字段"`
	OrderDirection string `json:"order_direction" type:"select" options:"ASC,DESC" default:"DESC" help:"排序方向"`
	RapidUpload    bool   `json:"rapid_upload" default:"true" help:"启用秒传功能(基于SHA1去重)"`
}

var config = driver.Config{
	Name:        "XiaomiCloud",
	LocalSort:   false,
	OnlyProxy:   false,
	NoCache:     false,
	NoUpload:    false,
	NeedMs:      false,
	DefaultRoot: "0",
	CheckStatus: false,
	Alert: `info|小米云盘存储驱动

配置方式：
1. 登录 https://i.mi.com/drive/h5
2. 打开浏览器开发者工具的 Network/网络 标签页并刷新页面
3. 找到 account.xiaomi.com/pass/serviceLogin 请求
4. 复制请求头中的完整 Cookie 到 account_cookie 字段
5. Cookie 必须包含 passToken；serviceToken 将由驱动自动获取和续签

特性：
- 自动获取并续签小米云盘 serviceToken
- 支持秒传功能(相同SHA1的文件无需重复上传)
- 支持大文件分块上传(4MB块)
- 支持CDN加速下载
- 支持完整的文件管理操作`,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &XiaomiCloud{}
	})
}
