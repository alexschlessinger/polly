package llm

import (
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// A run owns this cache. Tool schemas remain publicly mutable, so pointer or
// registry identity is insufficient: compare each current JSON tree against an
// owned snapshot, then serialize and estimate only changed trees.
type requestShapeCache struct {
	systems       []string
	tools         []*cachedRequestSchema
	toolTokens    int
	toolsPrepared bool
	response      *cachedRequestSchema
	shape         promptCacheShape
	key           string
	keyValid      bool
}

type cachedRequestSchema struct {
	present   bool
	strict    bool
	name      string
	snapshot  map[string]any
	cacheable bool
	raw       json.RawMessage
	err       error
	tokens    int
	toolJSON  []byte
}

func newRequestShapeCache(history []messages.ChatMessage) *requestShapeCache {
	c := &requestShapeCache{}
	for _, msg := range history {
		if msg.Role == messages.MessageRoleSystem {
			c.systems = append(c.systems, msg.GetContent())
		}
	}
	return c
}

func (c *requestShapeCache) prepareTools(list []tools.Tool) {
	old := c.tools
	if len(old) != len(list) {
		c.tools = make([]*cachedRequestSchema, len(list))
		c.keyValid = false
	}
	c.toolTokens = 0
	for i, tool := range list {
		schema := tool.GetSchema()
		var prior *cachedRequestSchema
		if i < len(old) {
			prior = old[i]
		}
		var entry *cachedRequestSchema
		if schema == nil {
			entry = updateRequestSchema(prior, false, false, "", nil)
		} else {
			entry = updateRequestSchema(prior, true, schema.Strict, schema.Title(), schema.Raw)
		}
		if entry != prior {
			c.keyValid = false
		}
		c.tools[i] = entry
		c.toolTokens += entry.tokens
	}
	c.toolsPrepared = true
}

func estimateRequestToolSchemaTokens(req *CompletionRequest) int {
	if req.shapeCache != nil && req.shapeCache.toolsPrepared {
		return req.shapeCache.toolTokens
	}
	return estimateToolSchemaTokens(req.Tools)
}

func updateRequestSchema(old *cachedRequestSchema, present, strict bool, name string, raw map[string]any) *cachedRequestSchema {
	if old != nil && old.cacheable && old.present == present && old.strict == strict && old.name == name && reflect.DeepEqual(old.snapshot, raw) {
		return old
	}
	entry := &cachedRequestSchema{present: present, strict: strict, name: name}
	entry.raw, entry.err = json.Marshal(raw)
	if entry.err == nil {
		if snapshot, ok := snapshotSchemaValue(reflect.ValueOf(raw)); ok {
			entry.snapshot = snapshot.Interface().(map[string]any)
			entry.cacheable = true
		}
	}
	if present {
		entry.tokens = 8
		if entry.err == nil {
			entry.tokens += estimatedJSONTokens(string(entry.raw))
		}
	}
	entry.toolJSON, _ = json.Marshal(promptCacheTool{Name: name, Strict: strict, Raw: entry.raw})
	return entry
}

func (c *requestShapeCache) promptCacheKey(req *CompletionRequest) (string, error) {
	prior := c.response
	if req.ResponseSchema == nil {
		c.response = nil
	} else {
		c.response = updateRequestSchema(prior, true, req.ResponseSchema.Strict, "", req.ResponseSchema.Raw)
	}
	if prior != c.response {
		c.keyValid = false
	}
	if c.keyValid && c.shape.Model == req.Model && c.shape.MaxTokens == req.MaxTokens &&
		c.shape.ThinkingEffort == req.ThinkingEffort.String() && reflect.DeepEqual(c.shape.Temperature, req.Temperature) {
		return c.key, nil
	}
	shape := promptCacheShape{
		Version: promptCacheKeyVersion, Model: req.Model, System: c.systems,
		MaxTokens: req.MaxTokens, ThinkingEffort: req.ThinkingEffort.String(),
	}
	if req.Temperature != nil {
		v := *req.Temperature
		shape.Temperature = &v
	}
	ordered := append([]*cachedRequestSchema(nil), c.tools...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].name != ordered[j].name {
			return ordered[i].name < ordered[j].name
		}
		return bytes.Compare(ordered[i].toolJSON, ordered[j].toolJSON) < 0
	})
	for _, entry := range ordered {
		if entry.err != nil {
			return "", entry.err
		}
		shape.Tools = append(shape.Tools, promptCacheTool{Name: entry.name, Strict: entry.strict, Raw: entry.raw})
	}
	if c.response != nil {
		if c.response.err != nil {
			return "", c.response.err
		}
		shape.ResponseSchema = &promptCacheSchema{Strict: c.response.strict, Raw: c.response.raw}
	}
	encoded, err := json.Marshal(shape)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	c.shape, c.key, c.keyValid = shape, hex.EncodeToString(digest[:]), true
	return c.key, nil
}

// Preserve concrete JSON-tree types so equality works for both []string and
// decoded []any schemas. Custom marshalers and non-JSON nodes are deliberately
// uncached: their output need not be determined by a structural snapshot.
func snapshotSchemaValue(v reflect.Value) (reflect.Value, bool) {
	if !v.IsValid() {
		return v, true
	}
	if v.CanInterface() {
		if _, custom := v.Interface().(json.Marshaler); custom {
			return reflect.Value{}, false
		}
		if _, custom := v.Interface().(encoding.TextMarshaler); custom {
			return reflect.Value{}, false
		}
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type()), true
		}
		inner, ok := snapshotSchemaValue(v.Elem())
		if !ok {
			return reflect.Value{}, false
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(inner)
		return out, true
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type()), true
		}
		if v.Type().Key().Kind() != reflect.String {
			return reflect.Value{}, false
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			value, ok := snapshotSchemaValue(iter.Value())
			if !ok {
				return reflect.Value{}, false
			}
			out.SetMapIndex(iter.Key(), value)
		}
		return out, true
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return reflect.Zero(v.Type()), true
		}
		var out reflect.Value
		if v.Kind() == reflect.Slice {
			out = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		} else {
			out = reflect.New(v.Type()).Elem()
		}
		for i := 0; i < v.Len(); i++ {
			value, ok := snapshotSchemaValue(v.Index(i))
			if !ok {
				return reflect.Value{}, false
			}
			out.Index(i).Set(value)
		}
		return out, true
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return v, true
	default:
		return reflect.Value{}, false
	}
}
