// Package audit 提供操作审计能力：记录谁（客户端 IP）在何时对哪个目标
// 执行了什么操作、结果如何。
//
// 审计是尽力而为的旁路能力：Recorder.Record 永不返回错误、永不 panic 外溢、
// 永不阻断调用方。审计自身的故障只会记录一条错误日志，绝不影响业务操作。
package audit

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// Outcome 审计结果，取值固定为 OutcomeSuccess 或 OutcomeFailure。
type Outcome string

const (
	// OutcomeSuccess 表示操作成功。
	OutcomeSuccess Outcome = "success"
	// OutcomeFailure 表示操作失败，失败原因见 Event.Reason。
	OutcomeFailure Outcome = "failure"
)

// Event 描述一次审计记录。
// 调用方填写已知字段，未填字段由 Recorder 兜底：
// Time 为零时取当前时间，Target 由 Recorder 统一截断。
// 后续扩展一律写入 Detail，不得新增顶层字段。
type Event struct {
	Time      time.Time      // 操作时间；为零时由 Recorder 填充
	ClientIP  string         // 客户端 IP，由 IPResolver 解析
	Operation string         // 操作标识，取值见包内 Operation 命名规范
	Target    string         // 操作对象：路径或命令；由 Recorder 统一截断
	Outcome   Outcome        // 成功或失败
	Reason    string         // 失败原因；成功时留空
	Bytes     int64          // 读写字节数；不适用时为 0
	Duration  time.Duration  // 操作耗时
	Detail    map[string]any // 可选扩展字段，输出时展开为 detail.<key>
}

// Recorder 审计记录器。
//
// 契约：Record 永不返回错误、永不 panic 外溢、永不阻断调用方。
// 未启用审计时 Enabled 返回 false，Record 为空操作。
type Recorder interface {
	// Enabled 返回是否启用审计。
	Enabled() bool
	// Record 输出一条审计记录，实现必须吞掉一切内部错误。
	Record(e Event)
}

// Options 构造 Recorder 的参数。
type Options struct {
	// Enabled 是否启用审计，对应 logging.audit_enabled。
	Enabled bool
	// TargetLimit 为 Event.Target 的截断字节上限；小于等于 0 时使用 DefaultTargetLimit。
	TargetLimit int
}

// DefaultTargetLimit 为 Event.Target 的默认截断字节上限。
const DefaultTargetLimit = 512

// recorder 是基于 slog 的 Recorder 实现，以 slog 属性形式输出审计记录。
// 审计记录以 audit=true 属性标记，可与业务日志区分和过滤。
type recorder struct {
	logger *slog.Logger
	limit  int
}

// NewRecorder 基于给定 logger 构造 Recorder。
// logger 决定输出去向，调用方传入 slog.Default() 即输出到 logging.file。
// opts.Enabled 为 false 时返回空实现。
func NewRecorder(logger *slog.Logger, opts Options) Recorder {
	if !opts.Enabled {
		return Noop()
	}
	if logger == nil {
		logger = slog.Default()
	}
	limit := opts.TargetLimit
	if limit <= 0 {
		limit = DefaultTargetLimit
	}
	return &recorder{logger: logger, limit: limit}
}

// noopRecorder 是审计关闭时使用的空实现，避免调用方做 nil 判断。
type noopRecorder struct{}

// Noop 返回始终不记录的 Recorder。
func Noop() Recorder { return noopRecorder{} }

func (noopRecorder) Enabled() bool { return false }

func (noopRecorder) Record(Event) {}

func (r *recorder) Enabled() bool { return true }

// Record 输出一条审计记录。
// 通过 recover 兜底，确保任何内部异常都不会外溢到业务调用方。
func (r *recorder) Record(e Event) {
	defer func() {
		if rec := recover(); rec != nil {
			// 审计自身故障：仅记录，不影响业务，也不会再次触发审计。
			r.logger.Error("audit record failed", "error", fmt.Sprint(rec))
		}
	}()
	r.write(e)
}

// write 将 Event 转换为 slog 属性并输出。
func (r *recorder) write(e Event) {
	ts := e.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	target, truncated := TruncateUTF8(e.Target, r.limit)

	attrs := make([]slog.Attr, 0, 11)
	attrs = append(attrs,
		slog.Bool("audit", true),
		slog.Time("time", ts),
		slog.String("client_ip", e.ClientIP),
		slog.String("op", e.Operation),
		slog.String("target", target),
		slog.String("outcome", string(e.Outcome)),
	)
	if truncated {
		attrs = append(attrs, slog.Bool("target_truncated", true))
	}
	if e.Reason != "" {
		attrs = append(attrs, slog.String("reason", e.Reason))
	}
	if e.Bytes != 0 {
		attrs = append(attrs, slog.Int64("bytes", e.Bytes))
	}
	if e.Duration > 0 {
		attrs = append(attrs, slog.Int64("duration_ms", e.Duration.Milliseconds()))
	}
	attrs = append(attrs, detailAttrs(e.Detail)...)

	r.logger.LogAttrs(context.Background(), slog.LevelInfo, "audit", attrs...)
}

// detailAttrs 将 Detail 展开为 detail.<key> 属性，按 key 排序保证输出稳定。
func detailAttrs(detail map[string]any) []slog.Attr {
	if len(detail) == 0 {
		return nil
	}
	keys := make([]string, 0, len(detail))
	for k := range detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	attrs := make([]slog.Attr, 0, len(keys))
	for _, k := range keys {
		attrs = append(attrs, slog.Any("detail."+k, detail[k]))
	}
	return attrs
}
