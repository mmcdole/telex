package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/gotd/td/constant"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// chatFilter implements TELEX_ALLOWED_CHATS. A nil or empty filter allows all.
type chatFilter struct {
	usernames map[string]bool // fixed after construction

	mu  sync.RWMutex
	ids map[int64]bool // grows as allow-listed usernames resolve
}

func newChatFilter(entries []string) (*chatFilter, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	f := &chatFilter{ids: map[int64]bool{}, usernames: map[string]bool{}}
	for _, e := range entries {
		if id, err := strconv.ParseInt(e, 10, 64); err == nil {
			f.ids[id] = true
			continue
		}
		u := strings.ToLower(strings.TrimPrefix(e, "@"))
		if u == "" || strings.ContainsAny(u, " /") {
			return nil, fmt.Errorf("TELEX_ALLOWED_CHATS: %q is neither a marked chat ID nor an @username", e)
		}
		f.usernames[u] = true
	}
	return f, nil
}

func (f *chatFilter) allowsID(id int64) bool {
	if f == nil {
		return true
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.ids[id]
}

func (f *chatFilter) addID(id int64) {
	f.mu.Lock()
	f.ids[id] = true
	f.mu.Unlock()
}

func (f *chatFilter) allows(p peers.Peer) bool {
	if f == nil {
		return true
	}
	id := int64(p.TDLibPeerID())
	if f.allowsID(id) {
		return true
	}
	if u, ok := p.Username(); ok && f.usernames[strings.ToLower(u)] {
		// Remember the ID so events from this chat pass the ID-only check.
		f.addID(id)
		return true
	}
	return false
}

// prime resolves allow-listed usernames to IDs up front, so events from them
// are recorded before any tool has touched the chat.
func (f *chatFilter) prime(ctx context.Context, m *peers.Manager) {
	if f == nil {
		return
	}
	for u := range f.usernames {
		if p, err := m.ResolveDomain(ctx, u); err == nil {
			f.addID(int64(p.TDLibPeerID()))
		}
	}
}

// resolveChat turns an agent-supplied chat reference into a peer:
//   - "me" / "self" / "saved" → Saved Messages
//   - a marked ID from list_chats (123 user, -123 group, -100123 channel)
//   - @username, t.me/… link, or +phone of a contact
//
// Display names are deliberately not accepted: guessing between two "Sam"s
// is how a message reaches the wrong person.
func (t *Telegram) resolveChat(ctx context.Context, ref string) (peers.Peer, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("chat is required")
	}
	var (
		p   peers.Peer
		err error
	)
	switch {
	case strings.EqualFold(ref, "me"), strings.EqualFold(ref, "self"), strings.EqualFold(ref, "saved"):
		p, err = t.peers.Self(ctx)
	case isInt(ref):
		id, _ := strconv.ParseInt(ref, 10, 64)
		p, err = t.peers.ResolveTDLibID(ctx, constant.TDLibPeerID(id))
		var nf *peers.PeerNotFoundError
		if errors.As(err, &nf) || tgerr.Is(err, tg.ErrUserIDInvalid, tg.ErrChannelInvalid, tg.ErrChatIDInvalid, tg.ErrPeerIDInvalid) {
			err = fmt.Errorf("chat %s is not known to this session; call list_chats (or use an @username) first", ref)
		}
	case strings.HasPrefix(ref, "@"), strings.Contains(ref, "t.me/"), strings.HasPrefix(ref, "tg:"), strings.HasPrefix(ref, "+"):
		p, err = t.peers.Resolve(ctx, ref)
	default:
		return nil, fmt.Errorf("chat %q: use a chat_id from list_chats, an @username, a t.me link, or \"me\" (names are not matched)", ref)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve chat %q: %w", ref, err)
	}
	if !t.allow.allows(p) {
		return nil, fmt.Errorf("chat %q is not in TELEX_ALLOWED_CHATS", ref)
	}
	return p, nil
}

func isInt(s string) bool {
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}
