package audit

import (
	"net/http"
	"net/url"
	"strings"
)

// redactor 只处理鉴权类 Header / 查询参数，且只作用于归档副本，绝不修改真实请求。
type redactor struct {
	headers map[string]struct{}
	query   map[string]struct{}
}

func newRedactor(cfg Config) *redactor {
	q := make(map[string]struct{}, len(cfg.RedactQueryParams))
	for _, p := range cfg.RedactQueryParams {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			q[p] = struct{}{}
		}
	}
	return &redactor{headers: canonicalSet(cfg.RedactHeaders), query: q}
}

// MaskSecret 保留前后各 4 位，中间以 * 替换；长度不足 9 时全部遮蔽，
// 以保证原值不会通过前后缀拼回。对 "Bearer xxx" 这类带方案前缀的值只遮蔽凭据部分。
func MaskSecret(value string) string {
	if value == "" {
		return ""
	}
	scheme := ""
	cred := value
	if i := strings.IndexByte(value, ' '); i > 0 && i < 16 && !strings.ContainsAny(value[:i], "=;,") {
		scheme, cred = value[:i+1], value[i+1:]
	}
	return scheme + maskToken(cred)
}

func maskToken(v string) string {
	r := []rune(v)
	if len(r) <= 8 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:4]) + strings.Repeat("*", len(r)-8) + string(r[len(r)-4:])
}

// Header 返回脱敏后的 Header 副本（多值保持顺序）。
func (r *redactor) Header(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		cp := make([]string, len(vs))
		_, sensitive := r.headers[strings.ToLower(k)]
		for i, v := range vs {
			if sensitive {
				cp[i] = r.maskHeaderValue(k, v)
			} else {
				cp[i] = v
			}
		}
		out[k] = cp
	}
	return out
}

func (r *redactor) maskHeaderValue(key, v string) string {
	switch strings.ToLower(key) {
	case "cookie":
		return maskPairs(v, ";", "=")
	case "set-cookie":
		// 仅遮蔽 name=value 的 value，保留属性（Path/Expires 等）。
		parts := strings.SplitN(v, ";", 2)
		masked := maskPairs(parts[0], ";", "=")
		if len(parts) == 2 {
			return masked + ";" + parts[1]
		}
		return masked
	default:
		return MaskSecret(v)
	}
}

func maskPairs(v, sep, kv string) string {
	items := strings.Split(v, sep)
	for i, item := range items {
		if j := strings.Index(item, kv); j >= 0 {
			items[i] = item[:j+1] + maskToken(item[j+1:])
		} else {
			items[i] = maskToken(item)
		}
	}
	return strings.Join(items, sep)
}

// URL 返回脱敏后的 URL 字符串；仅替换鉴权类查询参数，其余原样保留（含编码与顺序）。
func (r *redactor) URL(u *url.URL) string {
	if u == nil {
		return ""
	}
	if u.RawQuery == "" || len(r.query) == 0 {
		return u.String()
	}
	parts := strings.Split(u.RawQuery, "&")
	changed := false
	for i, p := range parts {
		name, val, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		decoded, err := url.QueryUnescape(name)
		if err != nil {
			decoded = name
		}
		if _, hit := r.query[strings.ToLower(decoded)]; hit {
			raw, err := url.QueryUnescape(val)
			if err != nil {
				raw = val
			}
			parts[i] = name + "=" + url.QueryEscape(maskToken(raw))
			changed = true
		}
	}
	if !changed {
		return u.String()
	}
	cp := *u
	cp.RawQuery = strings.Join(parts, "&")
	return cp.String()
}
