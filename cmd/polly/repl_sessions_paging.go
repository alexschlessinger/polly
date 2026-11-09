package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

const sessionsPickerPageSize = 64

const (
	pickerFirst = iota
	pickerMore
	pickerRefresh
	pickerSearch
)

type pickerBatch struct {
	rows    []sessions.SessionSummary
	deleted []string
	next    string
	legacy  bool
}

// Native requests select roots before decoding metadata. Search deliberately
// walks bounded root pages: arbitrary substring matching has catalogue cost,
// but never blocks the event loop or silently limits the searchable result.
// Legacy stores are listed once per refresh, not once per compatibility page.
func readPickerBatch(ctx context.Context, store sessions.SessionStore, after string, ids []string, preferred string) (pickerBatch, error) {
	var out pickerBatch
	if err := ctx.Err(); err != nil {
		return out, err
	}
	pages, native := store.(sessions.SummaryPageStore)
	if !native {
		rows, err := store.ListSummaries(ctx)
		if err != nil {
			return out, err
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		out.legacy = true
		for _, row := range rows {
			if row.Metadata != nil && (row.ParentID == "" && row.Metadata.Parent == "" || row.ID == preferred || row.Metadata.Name == preferred) {
				out.rows = append(out.rows, row)
			}
		}
		sort.SliceStable(out.rows, func(i, j int) bool {
			a, b := out.rows[i], out.rows[j]
			if !a.Metadata.LastUsed.Equal(b.Metadata.LastUsed) {
				return a.Metadata.LastUsed.After(b.Metadata.LastUsed)
			}
			if a.Metadata.Name != b.Metadata.Name {
				return a.Metadata.Name < b.Metadata.Name
			}
			return a.ID < b.ID
		})
		return out, ctx.Err()
	}
	root := ""
	page, err := pages.ListSummaryPage(ctx, sessions.SummaryQuery{ParentID: &root, After: after, Limit: sessionsPickerPageSize})
	if err != nil {
		return out, err
	}
	if page.Next != "" && page.Next == after {
		return out, fmt.Errorf("session summary cursor did not advance")
	}
	out.rows, out.next = page.Summaries, page.Next
	seen := make(map[string]bool, len(out.rows))
	for _, row := range out.rows {
		seen[row.ID] = true
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return pickerBatch{}, err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		row, err := pages.GetSummary(ctx, id)
		if errors.Is(err, sessions.ErrSessionNotFound) {
			out.deleted = append(out.deleted, id)
		} else if err != nil {
			return pickerBatch{}, err
		} else if row != nil {
			out.rows = append(out.rows, *row)
		}
	}
	if preferred != "" && !seen[preferred] {
		// A preferred old/off-page handle may be a child (not displayed in this
		// root-only picker). Preserve its exact identity for existing callers.
		if raw, err := hex.DecodeString(preferred); err == nil && len(raw) == 16 {
			row, err := pages.GetSummary(ctx, preferred)
			if err != nil && !errors.Is(err, sessions.ErrSessionNotFound) {
				return pickerBatch{}, err
			}
			if err == nil && row != nil {
				out.rows = append(out.rows, *row)
			}
		} else if headers, ok := store.(sessions.ViewHeaderStore); ok {
			header, err := headers.ReadViewHeader(ctx, sessions.ViewTarget{Name: preferred}, "")
			if err != nil && !errors.Is(err, sessions.ErrSessionNotFound) {
				return pickerBatch{}, err
			}
			if err == nil && header != nil {
				out.rows = append(out.rows, sessions.SessionSummary{ID: header.ID, ParentID: header.ParentID, Metadata: header.Metadata, MessageCount: int(header.Sequence), InUse: header.InUse})
			}
		}
	}
	return out, ctx.Err()
}

func (p *sessionsPicker) filter(text string) []replModalItem {
	key := strings.ToLower(strings.TrimSpace(text))
	if p.filterValid && key == p.filterKey {
		return p.filtered
	}
	p.filterKey, p.filterValid = key, true
	if key == "" {
		p.filtered = p.modal.items
		return p.filtered
	}
	p.filtered = nil
	p.filterIndex = make(map[string]int)
	for _, item := range p.modal.items {
		if strings.Contains(item.searchText, key) {
			p.filterIndex[item.identity] = len(p.filtered)
			p.filtered = append(p.filtered, item)
		}
	}
	return p.filtered
}

func (r *managedREPL) loadSessionsPickerPage(p *sessionsPicker, m *replModal, kind int) {
	store, owner, ctx, generation := r.state.sessionStore, r.model, p.ctx, p.generation
	query := p.query
	after := ""
	if kind == pickerMore {
		after = p.after
	} else if kind == pickerSearch {
		after = p.searchNext
	}
	var ids []string
	if kind == pickerFirst || kind == pickerRefresh {
		items := m.filteredItems()
		start := max(0, min(m.top, len(items)))
		end := min(len(items), start+min(sessionsPickerPageSize, max(16, m.visible)))
		for _, item := range items[start:end] {
			ids = append(ids, item.identity)
		}
	}
	preferred := ""
	if !p.started {
		preferred = p.preferred
	}
	p.listing = true
	m.titleNoticeRole = "muted"
	if kind == pickerSearch {
		m.titleNotice = "searching…"
	} else {
		m.titleNotice = "loading…"
	}
	if !r.background(func() {
		batch, err := readPickerBatch(ctx, store, after, ids, preferred)
		r.postUI(ctx, func() {
			owner.mu.Lock()
			defer owner.mu.Unlock()
			// A closed/replaced modal, changed filter, or moved workspace must
			// not accept a late result, even from a store ignoring cancellation.
			if ctx.Err() != nil || generation != p.generation || owner != r.model || owner.modal != m || p.query != strings.ToLower(strings.TrimSpace(m.input.text())) {
				return
			}
			p.listing = false
			p.listedAt = time.Now()
			if err != nil {
				m.titleNotice, m.titleNoticeRole = "load failed · "+err.Error(), "error"
				// Retry on the normal refresh interval, not on every paint.
				p.failed = true
				return
			}
			p.failed = false
			selected := pickerSelection(m)
			if !p.started && preferred != "" {
				for _, row := range batch.rows {
					if row.Metadata != nil && row.ParentID == "" && (row.ID == preferred || row.Metadata.Name == preferred) {
						selected = row.ID
					}
				}
			}
			search := ""
			if kind == pickerSearch {
				search = query
			}
			p.applyBatch(batch, search)
			if batch.legacy {
				p.after, p.done, p.searchDone = "", true, true
			} else if kind == pickerSearch {
				p.searchNext, p.searchDone = batch.next, batch.next == ""
			} else if kind != pickerRefresh || !p.started {
				p.after, p.done = batch.next, batch.next == ""
			}
			p.started = true
			if batch.legacy {
				r.refreshSessionsPickerItems(p, m, selected)
			} else {
				p.selectIdentity(selected)
			}
			m.titleNotice = ""
			if p.query != "" && !p.searchDone {
				// Yield through the UI queue between bounded search pages. A new
				// query cancels this generation; partial results stay usable.
				r.loadSessionsPickerPage(p, m, pickerSearch)
			} else if p.query == "" && !p.done {
				m.titleNotice = "scroll for more"
			}
		})
	}) {
		p.listing = false
	}
}

func (p *sessionsPicker) applyBatch(batch pickerBatch, search string) {
	if batch.legacy {
		p.merge(batch.rows, p.modal.expanded)
		return
	}
	for _, id := range batch.deleted {
		if old, ok := p.infos[id]; ok && p.byName[old.Metadata.Name] == id {
			delete(p.byName, old.Metadata.Name)
		}
		delete(p.infos, id)
		p.removeItem(id)
	}
	for _, row := range batch.rows {
		if row.Metadata == nil {
			continue
		}
		if _, exists := p.infos[row.ID]; !exists {
			if search != "" && !strings.Contains(strings.ToLower(row.Metadata.Title+" "+row.Metadata.Name+" "+row.Metadata.Description), search) {
				continue
			}
			p.rows = append(p.rows, sessionsPickerRow{id: row.ID})
		}
		if old, ok := p.infos[row.ID]; ok && old.Metadata.Name != row.Metadata.Name && p.byName[old.Metadata.Name] == row.ID {
			delete(p.byName, old.Metadata.Name)
		}
		p.infos[row.ID] = row
		p.byName[row.Metadata.Name] = row.ID
		if row.ParentID != "" || row.Metadata.Parent != "" {
			p.removeItem(row.ID)
		} else {
			p.updateItem(sessionPickerDescriptor(row))
		}
	}
	if len(batch.deleted) == 0 {
		return
	}
	rows := p.rows[:0]
	for _, row := range p.rows {
		if _, exists := p.infos[row.id]; exists {
			rows = append(rows, row)
		}
	}
	p.rows = rows
}

// Native page arrivals update only their selected identities. In particular a
// long search with many matches must not refilter/restyle every earlier page.
func (p *sessionsPicker) updateItem(item replModalItem) {
	if i, ok := p.itemIndex[item.identity]; ok {
		p.modal.items[i] = item
	} else {
		p.itemIndex[item.identity] = len(p.modal.items)
		p.modal.items = append(p.modal.items, item)
	}
	if !p.filterValid {
		return
	}
	if p.filterKey == "" {
		p.filtered = p.modal.items
		return
	}
	matches := strings.Contains(item.searchText, p.filterKey)
	if i, ok := p.filterIndex[item.identity]; ok {
		if matches {
			p.filtered[i] = item
		} else {
			p.filterValid = false
		}
	} else if matches {
		p.filterIndex[item.identity] = len(p.filtered)
		p.filtered = append(p.filtered, item)
	}
}

func (p *sessionsPicker) removeItem(id string) {
	i, ok := p.itemIndex[id]
	if !ok {
		return
	}
	delete(p.itemIndex, id)
	p.modal.items = slices.Delete(p.modal.items, i, i+1)
	for n := i; n < len(p.modal.items); n++ {
		p.itemIndex[p.modal.items[n].identity] = n
	}
	p.filterValid = false
}
