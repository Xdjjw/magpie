// Package audit is the audit plane, ported from xkeysmith's xp-audit
// (Rust): every upstream attempt becomes one self-describing record that
// can later be explained back into the escalation ladder.
//
// 这一层**不参与在线决策**,它出问题不会改变网关行为——审计不应成为
// 可用性的单点。本轮移植写侧(记录/脱敏/落盘/计数);读侧
// (explain / replay / stats,对应 xp-audit reader.rs)在接入展示 UI 时移植。
package audit

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

// ShortHash 取 SHA-256 的前 8 字节十六进制(16 字符):
// 审计里用它回答「这次尝试和上次是不是同一个请求」。
func ShortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// FullHash 是完整 SHA-256 十六进制,用于需要强比对(重放)的场合。
func FullHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ConstantTimeEq 用于令牌校验。
func ConstantTimeEq(a, b []byte) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}
