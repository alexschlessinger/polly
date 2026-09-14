// Package catalog holds what every provider's model listing shares: a
// bounded JSON fetch that never follows a redirect with credentials, the
// loosely typed accessors for provider records, the neutral fields of a
// model record, a page walker with cycle detection, and the listing
// collector that dedupes, filters, and sorts. Each provider package decides
// its paths, headers, and record shapes.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// maxBody bounds one metadata response.
const maxBody = 16 << 20

// maxPages bounds one listing walk.
const maxPages = 100

// FetchJSON performs one metadata request and decodes its JSON object.
// headers adds the provider's authentication. The request never follows a
// redirect, so credentials stay with the endpoint they were meant for. A
// non-2xx status or a body over 16 MiB is an error that carries no response
// content.
func FetchJSON(ctx context.Context, client *http.Client, method, uri string, body any, headers func(*http.Request)) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, uri, reader)
	if err != nil {
		return nil, fmt.Errorf("invalid metadata endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	if headers != nil {
		headers(req)
	}
	scoped := *client
	scoped.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := scoped.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("metadata request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("metadata endpoint returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBody {
		return nil, fmt.Errorf("model metadata exceeds 16 MiB")
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("invalid model metadata JSON: %w", err)
	}
	return out, nil
}

// Bearer returns a header setter for bearer-token authentication; an empty
// key sets nothing.
func Bearer(key string) func(*http.Request) {
	return func(r *http.Request) {
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
	}
}

// EscapeModelPath escapes each path segment of a model id, keeping the
// slashes that separate an organization from its model.
func EscapeModelPath(s string) string {
	parts := strings.Split(s, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// PageQuery appends a continuation token to a listing URL under param.
func PageQuery(uri, param, token string) string {
	if token == "" {
		return uri
	}
	q := url.Values{}
	q.Set(param, token)
	return uri + "?" + q.Encode()
}

// Loosely typed accessors for provider records; a wrong type reads as absent.

func Str(v any) string         { s, _ := v.(string); return s }
func Obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func Array(v any) []any        { a, _ := v.([]any); return a }
func Bool(v any) bool          { x, _ := v.(bool); return x }

// BoolPtr returns the boolean v holds, or nil when it is not a boolean.
func BoolPtr(v any) *bool {
	x, ok := v.(bool)
	if !ok {
		return nil
	}
	return &x
}

// IntPtr returns the non-negative number v holds, or nil.
func IntPtr(v any) *int {
	x, ok := v.(float64)
	if !ok || x < 0 {
		return nil
	}
	n := int(x)
	return &n
}

// Strings returns the sorted strings under key, or nil when the value is not
// a list.
func Strings(m map[string]any, key string) []string {
	v, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(v))
	for _, x := range v {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// Truth returns a pointer to v.
func Truth(v bool) *bool { return &v }

// BoundedRaw keeps a provider record inspectable without retaining unbounded
// model cards.
func BoundedRaw(r map[string]any) json.RawMessage {
	raw, _ := json.Marshal(r)
	if len(raw) > 64<<10 {
		return json.RawMessage(`{"truncated":true,"reason":"provider record exceeded 64 KiB"}`)
	}
	return raw
}

// AdvertisedFields copies the named keys a record advertises.
func AdvertisedFields(r map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := r[k]; ok {
			out[k] = v
		}
	}
	return out
}

// BaseModel decodes the fields any provider record may carry: id, name,
// description, lifecycle keys, architecture modalities, and token limits.
func BaseModel(r map[string]any) contract.ModelInfo {
	info := contract.ModelInfo{ID: Str(r["id"]), Name: Str(r["name"]), Description: Str(r["description"]), Raw: BoundedRaw(r), Lifecycle: map[string]any{}}
	for _, k := range []string{"created", "created_at", "version", "shutdown_date", "expiration_date", "deprecated", "preview", "owned_by"} {
		if v, ok := r[k]; ok {
			info.Lifecycle[k] = v
		}
	}
	arch := Obj(r["architecture"])
	info.InputModalities = Strings(arch, "input_modalities")
	info.OutputModalities = Strings(arch, "output_modalities")
	info.ContextTokens = IntPtr(r["context_length"])
	info.InputTokens = IntPtr(r["max_input_tokens"])
	info.OutputTokens = IntPtr(r["max_tokens"])
	return info
}

// NextPage returns the continuation token a listing page advertises: a
// nextPageToken, or last_id when has_more is set. has_more without last_id
// is an error.
func NextPage(raw map[string]any) (string, error) {
	next := Str(raw["nextPageToken"])
	if Bool(raw["has_more"]) {
		next = Str(raw["last_id"])
		if next == "" {
			return "", errors.New("model catalog pagination omitted last_id")
		}
	}
	return next, nil
}

// Walk calls page with each continuation token, starting empty, until a page
// advertises none. A repeated token or more than 100 pages is an error.
func Walk(page func(token string) (next string, err error)) error {
	visited := map[string]bool{}
	token := ""
	for range maxPages {
		next, err := page(token)
		if err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		if visited[next] {
			break
		}
		visited[next] = true
		token = next
	}
	return errors.New("model catalog pagination did not terminate")
}

// Listing accumulates one request's models: ids are deduplicated, a named
// lookup keeps only its own record, and malformed rows or a failed page mark
// the catalog partial.
type Listing struct {
	models  []contract.ModelInfo
	seen    map[string]bool
	partial bool
}

// Add keeps info unless its id is empty or already listed.
func (l *Listing) Add(info contract.ModelInfo) {
	if info.ID == "" || l.seen[info.ID] {
		return
	}
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	l.seen[info.ID] = true
	l.models = append(l.models, info)
}

// AddPage decodes one listing page: the rows under key or, when a named
// lookup was answered with a single object instead of a list, the object
// itself. A named lookup keeps only the matching id unless the page was
// that single object. A listing with no rows at all is unknown metadata.
func (l *Listing) AddPage(raw map[string]any, key, model string, decode func(map[string]any) contract.ModelInfo) error {
	rows := Array(raw[key])
	single := rows == nil && model != ""
	if single {
		rows = []any{raw}
	}
	if rows == nil {
		return contract.ErrModelMetadataUnknown
	}
	for _, value := range rows {
		row, ok := value.(map[string]any)
		if !ok {
			l.partial = true
			continue
		}
		info := decode(row)
		if single || model == "" || info.ID == model {
			l.Add(info)
		}
	}
	return nil
}

// Catalog returns the sorted catalog. A page failure is returned with
// whatever was collected, marked partial when anything was; a named lookup
// that found nothing reports unknown metadata.
func (l *Listing) Catalog(model string, err error) (contract.ModelCatalog, error) {
	cat := contract.ModelCatalog{Models: l.models, Partial: l.partial}
	if err != nil {
		cat.Partial = cat.Partial || len(cat.Models) > 0
		return cat, err
	}
	if model != "" && len(cat.Models) == 0 {
		return cat, contract.ErrModelMetadataUnknown
	}
	slices.SortFunc(cat.Models, func(a, b contract.ModelInfo) int { return strings.Compare(a.ID, b.ID) })
	return cat, nil
}
