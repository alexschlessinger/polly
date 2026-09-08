package main

import (
	"maps"
	"reflect"
	"slices"

	"github.com/alexschlessinger/pollytool/sessions"
)

const (
	childViewCacheBytes       = 64 << 20
	childViewCacheEntries     = 16
	childViewProbationBytes   = 8 << 20
	childViewProbationEntries = 4
)

// A cached child is a disposable display projection, not an open tab. Its
// model is built by childDisplayCopy and owns no runtime, lease, callbacks,
// drafts, or pending input; a prompt restored from saved history is display
// and stays. Taking an entry transfers it to the open view; open views are
// never cache eviction candidates.
type cachedChildView struct {
	key      string
	view     *viewInstance
	info     *sessions.SessionView
	model    *replModel
	used     uint64 // last user visit; zero is an unviewed completion
	admitted uint64
	bytes    int64
}

type ViewCache struct {
	entries    map[string]*cachedChildView
	clock      uint64
	bytes      int64
	maxBytes   int64
	maxEntries int
}

type childViewCache = ViewCache

func (v *cachedChildView) cacheKey() string {
	if v.key != "" {
		return v.key
	}
	return v.info.ID
}

func (c *childViewCache) visit() uint64 { c.clock++; return c.clock }

func (c *childViewCache) take(id string) *cachedChildView {
	v := c.entries[id]
	if v != nil {
		delete(c.entries, id)
		c.bytes -= v.bytes
	}
	return v
}

func (c *childViewCache) put(v *cachedChildView) {
	if v == nil || v.info == nil || v.info.ID == "" {
		return
	}
	if c.maxBytes == 0 {
		c.maxBytes = childViewCacheBytes
	}
	if c.maxEntries == 0 {
		c.maxEntries = childViewCacheEntries
	}
	// A late background completion must not replace a more recently visited
	// projection or change its recency.
	if old := c.entries[v.cacheKey()]; old != nil && old.used > v.used {
		return
	}
	if v.bytes > c.maxBytes || v.used == 0 && v.bytes > childViewProbationBytes {
		return
	}
	c.take(v.cacheKey())
	if c.entries == nil {
		c.entries = make(map[string]*cachedChildView)
	}
	c.entries[v.cacheKey()] = v
	c.clock++
	v.admitted = c.clock
	c.bytes += v.bytes
	for {
		var victim *cachedChildView
		var probationBytes int64
		probationCount := 0
		for _, entry := range c.entries {
			if entry.used == 0 {
				probationBytes += entry.bytes
				probationCount++
			}
			if victim == nil || entry.used < victim.used || entry.used == victim.used && entry.admitted < victim.admitted {
				victim = entry
			}
		}
		if victim == nil || c.bytes <= c.maxBytes && len(c.entries) <= c.maxEntries && probationBytes <= childViewProbationBytes && probationCount <= childViewProbationEntries {
			return
		}
		c.take(victim.cacheKey())
	}
}

// childDisplayCopy explicitly selects display state. In particular, copying
// replModel wholesale would retain approvals, queued input and live children.
// Caller holds src.mu. Rendered cell backing arrays are immutable; clone their
// mutable outer indexes so subsequent paints cannot patch the old model.
func childDisplayCopy(src *replModel) *replModel {
	m := newReplModel()
	m.inspections = src.inspections.clone()
	m.hidden, m.quiet = true, src.quiet
	m.imageBaseDir = src.imageBaseDir
	m.nativeImages, m.imageCellWidth, m.imageCellHeight = src.nativeImages, src.imageCellWidth, src.imageCellHeight
	m.transcript = slices.Clone(src.transcript)
	m.userPromptSeen, m.collapseInitialPrompt = src.userPromptSeen, src.collapseInitialPrompt
	m.initialPromptExpanded = src.initialPromptExpanded
	for i := range m.transcript {
		m.transcript[i].images = slices.Clone(m.transcript[i].images)
		m.transcript[i].codeCache = nil
	}
	m.markdownPending = src.markdownPending
	m.visual = src.visual
	m.visual.rows = slices.Clone(src.visual.rows)
	m.visual.blocks = slices.Clone(src.visual.blocks)
	if src.masthead.enabled {
		// The snapshot has no masthead; its cached rows were laid out with one.
		m.visual.invalidate()
	}
	m.followBottom, m.scrollAnchor = src.followBottom, src.scrollAnchor
	m.status = src.status
	m.status.recentModels = slices.Clone(src.status.recentModels)
	// Durable image token descriptions belong to history, not to a draft.
	// Keep their numbering without retaining unsubmitted local attachments.
	m.attachmentSeq = src.attachmentSeq
	m.ambiguousAttachments = maps.Clone(src.ambiguousAttachments)
	for id, attachment := range src.attachments {
		if attachment.Artifact == nil {
			continue
		}
		if m.attachments == nil {
			m.attachments = make(map[int]composerAttachment)
		}
		ref := *attachment.Artifact
		attachment.Artifact = &ref
		m.attachments[id] = attachment
	}
	// The unanswered prompt hydrateHistory restored derives from history,
	// so the projection shows it again; typed edits are not copied.
	if src.restoredDraft != nil {
		restored := cloneManagedTurn(*src.restoredDraft)
		m.restoredDraft, m.restoredPersistence = &restored, src.restoredPersistence
		m.ed.setText(restored.displayText)
	}
	m.lastIn, m.lastOut, m.lastElapsed, m.lastOutcome = src.lastIn, src.lastOut, src.lastElapsed, src.lastOutcome
	m.toolDisclosureAt = maps.Clone(src.toolDisclosureAt)
	m.toolDisclosureSeq = src.toolDisclosureSeq
	m.turnToolDisclosureIDs = slices.Clone(src.turnToolDisclosureIDs)
	for id, record := range src.toolDisclosures {
		copy := *record
		copy.rows = slices.Clone(record.rows)
		for i := range copy.rows {
			row := &copy.rows[i]
			row.images = slices.Clone(row.images)
			row.inspectionImages = slices.Clone(row.inspectionImages)
			if row.agent != nil {
				a := *row.agent
				a.origin = nil
				row.agent = &a
			}
		}
		m.toolDisclosures[id] = &copy
	}
	m.reasoningAt = maps.Clone(src.reasoningAt)
	m.reasoningOrder = slices.Clone(src.reasoningOrder)
	m.reasoningSeq, m.reasoningWidth = src.reasoningSeq, src.reasoningWidth
	m.turnReasoningIDs = slices.Clone(src.turnReasoningIDs)
	for id, record := range src.reasoningRecords {
		copy := *record
		copy.tail = slices.Clone(record.tail)
		copy.previewLines = slices.Clone(record.previewLines)
		m.reasoningRecords[id] = &copy
	}
	m.turnTrailerAt = maps.Clone(src.turnTrailerAt)
	m.turnTrailerSeq, m.openTurnTrailerID = src.turnTrailerSeq, src.openTurnTrailerID
	m.turnDock = cloneViewDock(src.turnDock)
	for id, record := range src.turnTrailers {
		copy := *record
		copy.dock = cloneViewDock(record.dock)
		copy.fields = slices.Clone(record.fields)
		m.turnTrailers[id] = &copy
	}
	return m
}

func cloneViewDock(d turnDockState) turnDockState {
	d.reasoningIDs = slices.Clone(d.reasoningIDs)
	d.toolIDs = slices.Clone(d.toolIDs)
	return d
}

// Estimate retained allocations conservatively, including slice capacity and
// rendered cells rather than just Markdown bytes. Shared allocations may be
// counted twice; the cache prefers early eviction to an understated budget.
// Only the isolated display model is measured, before attaching its store.
func childViewSize(m *replModel) int64 { return retainedViewBytes(reflect.ValueOf(m)) }

func retainedViewBytes(v reflect.Value) int64 {
	return retainedViewBytesPath(v, make(map[viewAllocation]bool))
}

type viewAllocation struct {
	typ     reflect.Type
	pointer uintptr
}

func retainedViewBytesPath(v reflect.Value, path map[viewAllocation]bool) int64 {
	if !v.IsValid() {
		return 0
	}
	// Metadata and standard-library structs can contain cycles (for example
	// local time zones). Count shared values conservatively, but never follow
	// a pointer already on this traversal path.
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if v.IsNil() {
			return 0
		}
		key := viewAllocation{v.Type(), uintptr(v.UnsafePointer())}
		if path[key] {
			return 0
		}
		path[key] = true
		defer delete(path, key)
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return 0
		}
		return int64(v.Type().Elem().Size()) + retainedViewBytesPath(v.Elem(), path)
	case reflect.Interface:
		if v.IsNil() {
			return 0
		}
		return retainedViewBytesPath(v.Elem(), path)
	case reflect.String:
		return int64(v.Len())
	case reflect.Slice:
		n := int64(v.Cap()) * int64(v.Type().Elem().Size())
		for i := 0; i < v.Len(); i++ {
			n += retainedViewBytesPath(v.Index(i), path)
		}
		return n
	case reflect.Map:
		n := int64(v.Len()) * (int64(v.Type().Key().Size()+v.Type().Elem().Size()) + 32)
		it := v.MapRange()
		for it.Next() {
			n += retainedViewBytesPath(it.Key(), path) + retainedViewBytesPath(it.Value(), path)
		}
		return n
	case reflect.Struct:
		var n int64
		for i := 0; i < v.NumField(); i++ {
			n += retainedViewBytesPath(v.Field(i), path)
		}
		return n
	}
	return 0
}
