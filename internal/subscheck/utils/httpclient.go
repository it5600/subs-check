package utils

import (
	"net/http"
	"time"
)

// OutboundHTTPClient 用于所有「应用主动发起」的 HTTP 调用：sub-store REST、
// Mihomo API、Apprise 通知。
//
// 此前这些调用点直接用 http.Get / http.Post / http.DefaultClient，而
// net/http 的默认客户端 Timeout=0 且不绑定 context。checkProxies() 会在
// 一轮检测末尾同步调用 UpdateSubs()/SendNotify()，任一目标挂起就会永久
// 阻塞整条检测流水线，连 POST /api/force-close 的取消都中断不了。
//
// 15s 只裁掉异常挂起；正常响应内容与顺序完全不变，也不改动任何对外接口。
// 与 subs-check-pro 的「通知/更新请求带超时 + 不阻塞主循环」实践对齐。
var OutboundHTTPClient = &http.Client{Timeout: 15 * time.Second}
