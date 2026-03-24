package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
)

var tplVarRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`)

func RenderTemplateBody(body string, payload []byte) (string, error) {
	if !tplVarRe.MatchString(body) {
		return body, nil
	}
	if len(payload) == 0 {
		return "", fmt.Errorf("empty payload")
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return "", err
	}
	out := tplVarRe.ReplaceAllStringFunc(body, func(match string) string {
		sub := tplVarRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		key := sub[1]
		v, ok := m[key]
		if !ok {
			return match
		}
		switch t := v.(type) {
		case string:
			return t
		case float64, bool, json.Number:
			return fmt.Sprint(t)
		default:
			b, err := json.Marshal(t)
			if err != nil {
				return match
			}
			return string(b)
		}
	})
	if tplVarRe.MatchString(out) {
		return "", fmt.Errorf("unresolved template variables")
	}
	return out, nil
}
