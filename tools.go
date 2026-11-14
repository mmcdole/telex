package main

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/telegram/query"
	qmessages "github.com/gotd/td/telegram/query/messages"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"
)

const maxTextLen = 4096 // Telegram's message length limit (caption: 1024, 4096 with Premium)

type toolset struct {
	t     *Telegram
	files *fileGuard
	audit *auditLog
	// Write actions are human-paced: bursts of 3, then one every 3 seconds.
	// Spam-like send patterns are what get user accounts restricted.
	sendLimit *rate.Limiter
}

func ptr[T any](v T) *T { return &v }

func register(s *mcp.Server, ts *toolset, readOnly bool) {
	read := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}
	write := &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}

	mcp.AddTool(s, &mcp.Tool{Name: "get_me", Annotations: read,
		Description: "Return the Telegram account this server acts as."}, ts.getMe)
	mcp.AddTool(s, &mcp.Tool{Name: "list_chats", Annotations: read,
		Description: "List dialogs, most recent first, with unread counts and the last message. Use the returned chat_id in other tools."}, ts.listChats)
	mcp.AddTool(s, &mcp.Tool{Name: "get_messages", Annotations: read,
		Description: "Read a chat's history, returned oldest first. Without before_id/after_id it returns the latest messages. Page back with before_id = the smallest id you have; read what is new with after_id = the largest. Does not mark anything read."}, ts.getMessages)
	mcp.AddTool(s, &mcp.Tool{Name: "search_messages", Annotations: read,
		Description: "Full-text search in one chat, or across all chats when chat is omitted. Newest first."}, ts.searchMessages)
	mcp.AddTool(s, &mcp.Tool{Name: "wait_for_messages", Annotations: read,
		Description: "Long-poll for new messages arriving on the account (incoming by default). First call: omit after_seq to start from now. Then pass back boot and last_seq each time. If reset or gap is true, re-read the chats you follow with get_messages."}, ts.waitForMessages)
	mcp.AddTool(s, &mcp.Tool{Name: "download_media", Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
		Description: "Download a message's photo or file to the local download directory and return its path."}, ts.downloadMedia)

	if readOnly {
		return
	}
	mcp.AddTool(s, &mcp.Tool{Name: "send_message", Annotations: write,
		Description: "Send a text message as the account owner. It is delivered immediately and appears as them; there is no undo."}, ts.sendMessage)
	mcp.AddTool(s, &mcp.Tool{Name: "send_file", Annotations: write,
		Description: "Send local files (1-10; several become an album) with an optional caption. Images go as photos unless as_document is set. Paths must be inside the allowed roots or the download directory."}, ts.sendFile)
	mcp.AddTool(s, &mcp.Tool{Name: "send_reaction", Annotations: write,
		Description: "React to a message with an emoji, or clear your reaction with an empty emoji."}, ts.sendReaction)
	mcp.AddTool(s, &mcp.Tool{Name: "mark_read", Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(true)},
		Description: "Mark a chat read (up to max_id, or everything). The sender sees read receipts."}, ts.markRead)
}

// friendly rewrites Telegram errors into something an agent can act on.
func friendly(err error) error {
	if err == nil {
		return nil
	}
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Errorf("telegram rate limit: wait %s before retrying this kind of request", d.Round(time.Second))
	}
	if rpc, ok := tgerr.As(err); ok {
		return fmt.Errorf("telegram error %s (%d): %w", rpc.Type, rpc.Code, err)
	}
	return err
}

func (ts *toolset) ready(ctx context.Context) error { return ts.t.Await(ctx) }

func clamp(v, def, lo, hi int) int {
	if v == 0 {
		return def
	}
	return max(lo, min(v, hi))
}

// ---- get_me ---------------------------------------------------------------

type Empty struct{}

type MeOut struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username,omitempty"`
	Premium  bool   `json:"premium"`
}

func (ts *toolset) getMe(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, MeOut, error) {
	if err := ts.ready(ctx); err != nil {
		return nil, MeOut{}, err
	}
	u := ts.t.Self()
	return nil, MeOut{ID: u.ID, Name: userName(u), Username: u.Username, Premium: u.Premium}, nil
}

// ---- list_chats -----------------------------------------------------------

type ListChatsIn struct {
	Limit      int    `json:"limit,omitempty" jsonschema:"max chats to return (default 30, max 200)"`
	UnreadOnly bool   `json:"unread_only,omitempty" jsonschema:"only chats with unread messages"`
	Kind       string `json:"kind,omitempty" jsonschema:"only this kind: user, bot, group, or channel"`
	Archived   bool   `json:"archived,omitempty" jsonschema:"list the archive folder instead of the main list"`
}

type Chat struct {
	ChatID         int64    `json:"chat_id"`
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	Username       string   `json:"username,omitempty"`
	Unread         int      `json:"unread"`
	UnreadMentions int      `json:"unread_mentions,omitempty"`
	Pinned         bool     `json:"pinned,omitempty"`
	Muted          bool     `json:"muted,omitempty"`
	Last           *Message `json:"last_message,omitempty"`
}

type ListChatsOut struct {
	Chats []Chat `json:"chats"`
}

func (ts *toolset) listChats(ctx context.Context, _ *mcp.CallToolRequest, in ListChatsIn) (*mcp.CallToolResult, ListChatsOut, error) {
	if err := ts.ready(ctx); err != nil {
		return nil, ListChatsOut{}, err
	}
	if in.Kind != "" && !slices.Contains([]string{"user", "bot", "group", "channel"}, in.Kind) {
		return nil, ListChatsOut{}, fmt.Errorf("kind must be user, bot, group, or channel")
	}
	limit := clamp(in.Limit, 30, 1, 200)
	q := query.GetDialogs(ts.t.api).BatchSize(100)
	if in.Archived {
		q = q.FolderID(1)
	}
	out := ListChatsOut{Chats: []Chat{}}
	applied := map[int64]bool{}
	now := int(time.Now().Unix())

	it := q.Iter()
	for it.Next(ctx) && len(out.Chats) < limit {
		el := it.Value()
		d, ok := el.Dialog.(*tg.Dialog)
		if !ok {
			continue // folder entry
		}
		ents := entities{users: el.Entities.Users(), chats: el.Entities.Chats(), channels: el.Entities.Channels()}
		ts.applyOnce(ctx, ents, applied)

		id := markedID(d.Peer)
		if !ts.t.allow.allowsID(id) {
			continue
		}
		c := Chat{
			ChatID:         id,
			Name:           ents.name(d.Peer),
			Kind:           ents.kind(d.Peer),
			Unread:         d.UnreadCount,
			UnreadMentions: d.UnreadMentionsCount,
			Pinned:         d.Pinned,
			Muted:          d.NotifySettings.MuteUntil > now,
		}
		if in.UnreadOnly && c.Unread == 0 && !d.UnreadMark {
			continue
		}
		if in.Kind != "" && c.Kind != in.Kind {
			continue
		}
		switch p := d.Peer.(type) {
		case *tg.PeerUser:
			if u, ok := ents.users[p.UserID]; ok {
				c.Username = u.Username
			}
		case *tg.PeerChannel:
			if ch, ok := ents.channels[p.ChannelID]; ok {
				c.Username = ch.Username
			}
		}
		if last, ok := el.Last.(tg.MessageClass); ok {
			if m, ok := convertMessage(last, ents, false); ok {
				m.Text = preview(m.Text, 200)
				c.Last = &m
			}
		}
		out.Chats = append(out.Chats, c)
	}
	if err := it.Err(); err != nil {
		return nil, ListChatsOut{}, friendly(err)
	}
	return nil, out, nil
}

// applyOnce stores access hashes from a dialogs page so the chat IDs we hand
// out resolve later, skipping entities already applied in this call.
func (ts *toolset) applyOnce(ctx context.Context, e entities, seen map[int64]bool) {
	var users []tg.UserClass
	var chats []tg.ChatClass
	for id, u := range e.users {
		if !seen[id] {
			seen[id] = true
			users = append(users, u)
		}
	}
	for id, c := range e.chats {
		if !seen[-id] {
			seen[-id] = true
			chats = append(chats, c)
		}
	}
	for id, c := range e.channels {
		if k := -1_000_000_000_000 - id; !seen[k] {
			seen[k] = true
			chats = append(chats, c)
		}
	}
	if len(users)+len(chats) > 0 {
		if err := ts.t.peers.Apply(ctx, users, chats); err != nil {
			ts.t.log.Warn("apply entities", "err", err)
		}
	}
}

// ---- get_messages ---------------------------------------------------------

type GetMessagesIn struct {
	Chat     string `json:"chat" jsonschema:"chat_id from list_chats, @username, t.me link, or me"`
	Limit    int    `json:"limit,omitempty" jsonschema:"default 20, max 100"`
	BeforeID int    `json:"before_id,omitempty" jsonschema:"return messages older than this id"`
	AfterID  int    `json:"after_id,omitempty" jsonschema:"return messages newer than this id"`
}

type MessagesOut struct {
	ChatID   int64     `json:"chat_id"`
	ChatName string    `json:"chat_name"`
	Messages []Message `json:"messages"`
	HasMore  bool      `json:"has_more" jsonschema:"more messages exist in the direction you paged"`
}

func (ts *toolset) getMessages(ctx context.Context, _ *mcp.CallToolRequest, in GetMessagesIn) (*mcp.CallToolResult, MessagesOut, error) {
	if err := ts.ready(ctx); err != nil {
		return nil, MessagesOut{}, err
	}
	if in.BeforeID != 0 && in.AfterID != 0 {
		return nil, MessagesOut{}, errors.New("pass before_id or after_id, not both")
	}
	p, err := ts.t.resolveChat(ctx, in.Chat)
	if err != nil {
		return nil, MessagesOut{}, err
	}
	limit := clamp(in.Limit, 20, 1, 100)
	req := &tg.MessagesGetHistoryRequest{Peer: p.InputPeer(), Limit: limit}
	switch {
	case in.BeforeID > 0:
		req.OffsetID = in.BeforeID
	case in.AfterID > 0:
		// The window of `limit` messages starting just after after_id:
		// shift the offset forward by limit, and cut at min_id.
		req.OffsetID, req.AddOffset, req.MinID = in.AfterID+1, -limit, in.AfterID
	}
	res, err := ts.t.api.MessagesGetHistory(ctx, req)
	if err != nil {
		return nil, MessagesOut{}, friendly(err)
	}
	msgs, ents, err := ts.unpack(ctx, res)
	if err != nil {
		return nil, MessagesOut{}, err
	}
	out := MessagesOut{ChatID: int64(p.TDLibPeerID()), ChatName: p.VisibleName(), Messages: []Message{}, HasMore: len(msgs) == limit}
	for _, m := range msgs {
		if cm, ok := convertMessage(m, ents, false); ok {
			out.Messages = append(out.Messages, cm)
		}
	}
	slices.SortFunc(out.Messages, func(a, b Message) int { return a.ID - b.ID })
	return nil, out, nil
}

// unpack extracts messages and entities from a messages.* response and feeds
// the entities to the peer cache.
func (ts *toolset) unpack(ctx context.Context, res tg.MessagesMessagesClass) ([]tg.MessageClass, entities, error) {
	mod, ok := res.AsModified()
	if !ok {
		return nil, entities{}, errors.New("unexpected messages.messagesNotModified")
	}
	users, chats := mod.GetUsers(), mod.GetChats()
	if err := ts.t.peers.Apply(ctx, users, chats); err != nil {
		ts.t.log.Warn("apply entities", "err", err)
	}
	return mod.GetMessages(), newEntities(users, chats), nil
}

// ---- search_messages ------------------------------------------------------

type SearchIn struct {
	Query string `json:"query" jsonschema:"text to search for"`
	Chat  string `json:"chat,omitempty" jsonschema:"limit to one chat (chat_id, @username, or me); omit to search all chats"`
	Limit int    `json:"limit,omitempty" jsonschema:"default 20, max 50"`
}

type SearchOut struct {
	Messages []Message `json:"messages"`
}

func (ts *toolset) searchMessages(ctx context.Context, _ *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, SearchOut, error) {
	if err := ts.ready(ctx); err != nil {
		return nil, SearchOut{}, err
	}
	if strings.TrimSpace(in.Query) == "" {
		return nil, SearchOut{}, errors.New("query is required")
	}
	limit := clamp(in.Limit, 20, 1, 50)
	var (
		res tg.MessagesMessagesClass
		err error
	)
	if in.Chat != "" {
		p, rerr := ts.t.resolveChat(ctx, in.Chat)
		if rerr != nil {
			return nil, SearchOut{}, rerr
		}
		res, err = ts.t.api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
			Peer: p.InputPeer(), Q: in.Query, Filter: &tg.InputMessagesFilterEmpty{}, Limit: limit,
		})
	} else {
		res, err = ts.t.api.MessagesSearchGlobal(ctx, &tg.MessagesSearchGlobalRequest{
			Q: in.Query, Filter: &tg.InputMessagesFilterEmpty{}, OffsetPeer: &tg.InputPeerEmpty{}, Limit: limit,
		})
	}
	if err != nil {
		return nil, SearchOut{}, friendly(err)
	}
	msgs, ents, err := ts.unpack(ctx, res)
	if err != nil {
		return nil, SearchOut{}, err
	}
	out := SearchOut{Messages: []Message{}}
	for _, m := range msgs {
		if cm, ok := convertMessage(m, ents, true); ok && ts.t.allow.allowsID(cm.ChatID) {
			out.Messages = append(out.Messages, cm)
		}
	}
	return nil, out, nil
}

// ---- wait_for_messages ----------------------------------------------------

type WaitIn struct {
	AfterSeq        *int64 `json:"after_seq,omitempty" jsonschema:"last_seq from the previous call; omit to start from now"`
	Boot            string `json:"boot,omitempty" jsonschema:"boot from the previous call"`
	Chat            string `json:"chat,omitempty" jsonschema:"only messages in this chat"`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait for a message (default 50, max 120)"`
	Limit           int    `json:"limit,omitempty" jsonschema:"max events to return (default 50, max 200)"`
	IncludeOutgoing bool   `json:"include_outgoing,omitempty" jsonschema:"also return messages this account sent (from any device)"`
}

func (ts *toolset) waitForMessages(ctx context.Context, _ *mcp.CallToolRequest, in WaitIn) (*mcp.CallToolResult, WaitResult, error) {
	if err := ts.ready(ctx); err != nil {
		return nil, WaitResult{}, err
	}
	var chatID int64
	if in.Chat != "" {
		p, err := ts.t.resolveChat(ctx, in.Chat)
		if err != nil {
			return nil, WaitResult{}, err
		}
		chatID = int64(p.TDLibPeerID())
	}
	after := int64(-1)
	if in.AfterSeq != nil {
		after = *in.AfterSeq
	}
	timeout := time.Duration(clamp(in.TimeoutSeconds, 50, 1, 120)) * time.Second
	res := ts.t.events.Wait(ctx, in.Boot, after, timeout, clamp(in.Limit, 50, 1, 200), func(e Event) bool {
		return (in.IncludeOutgoing || !e.Out) && (chatID == 0 || e.ChatID == chatID)
	})
	return nil, res, nil
}

// ---- download_media -------------------------------------------------------

type DownloadIn struct {
	Chat      string `json:"chat"`
	MessageID int    `json:"message_id"`
	MaxMB     int    `json:"max_mb,omitempty" jsonschema:"refuse files larger than this (default 100)"`
}

type DownloadOut struct {
	Path string `json:"path"`
	MIME string `json:"mime,omitempty"`
	Size int64  `json:"size"`
}

func (ts *toolset) downloadMedia(ctx context.Context, _ *mcp.CallToolRequest, in DownloadIn) (*mcp.CallToolResult, DownloadOut, error) {
	if err := ts.ready(ctx); err != nil {
		return nil, DownloadOut{}, err
	}
	p, err := ts.t.resolveChat(ctx, in.Chat)
	if err != nil {
		return nil, DownloadOut{}, err
	}
	msg, err := ts.getMessage(ctx, p, in.MessageID)
	if err != nil {
		return nil, DownloadOut{}, err
	}
	f, ok := qmessages.Elem{Msg: msg, Peer: p.InputPeer()}.File()
	if !ok {
		return nil, DownloadOut{}, fmt.Errorf("message %d has no downloadable photo or file", in.MessageID)
	}
	if md := convertMedia(msg.Media); md != nil && md.Size > int64(clamp(in.MaxMB, 100, 1, 2000))<<20 {
		return nil, DownloadOut{}, fmt.Errorf("file is %d MB; raise max_mb to download it", md.Size>>20)
	}
	name := f.Name
	if name == "" || filepath.Ext(name) == "" {
		name = fmt.Sprintf("%d_%d%s", p.ID(), in.MessageID, extForMIME(f.MIMEType))
	}
	path, err := ts.files.downloadPath(name)
	if err != nil {
		return nil, DownloadOut{}, err
	}
	if _, err := downloader.NewDownloader().Download(ts.t.api, f.Location).ToPath(ctx, path); err != nil {
		_ = os.Remove(path)
		return nil, DownloadOut{}, friendly(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, DownloadOut{}, err
	}
	return nil, DownloadOut{Path: path, MIME: f.MIMEType, Size: st.Size()}, nil
}

func (ts *toolset) getMessage(ctx context.Context, p peers.Peer, id int) (*tg.Message, error) {
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: id}}
	var (
		res tg.MessagesMessagesClass
		err error
	)
	if ch, ok := p.InputPeer().(*tg.InputPeerChannel); ok {
		res, err = ts.t.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash}, ID: ids,
		})
	} else {
		res, err = ts.t.api.MessagesGetMessages(ctx, ids)
	}
	if err != nil {
		return nil, friendly(err)
	}
	msgs, _, err := ts.unpack(ctx, res)
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if m, ok := m.(*tg.Message); ok && m.ID == id {
			// messages.getMessages is not scoped to a chat; make sure the
			// message really belongs to the one asked about.
			if markedID(m.PeerID) != int64(p.TDLibPeerID()) {
				break
			}
			return m, nil
		}
	}
	return nil, fmt.Errorf("message %d not found in this chat", id)
}

// ---- send_message ---------------------------------------------------------

type SendIn struct {
	Chat    string `json:"chat" jsonschema:"chat_id from list_chats, @username, t.me link, or me"`
	Text    string `json:"text" jsonschema:"message text (max 4096 characters)"`
	ReplyTo int    `json:"reply_to,omitempty" jsonschema:"message id to reply to"`
	Format  string `json:"format,omitempty" jsonschema:"plain (default) or html: <b> <i> <u> <s> <code> <pre> <a href> <blockquote> <tg-spoiler>"`
	Silent  bool   `json:"silent,omitempty" jsonschema:"deliver without a notification sound"`
}

type SentOut struct {
	ChatID     int64  `json:"chat_id"`
	ChatName   string `json:"chat_name"`
	MessageIDs []int  `json:"message_ids"`
}

func (ts *toolset) sendMessage(ctx context.Context, req *mcp.CallToolRequest, in SendIn) (*mcp.CallToolResult, SentOut, error) {
	if err := ts.ready(ctx); err != nil {
		return nil, SentOut{}, err
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil, SentOut{}, errors.New("text is required")
	}
	if n := utf8.RuneCountInString(in.Text); n > maxTextLen {
		return nil, SentOut{}, fmt.Errorf("text is %d characters; Telegram's limit is %d, so split it into several messages", n, maxTextLen)
	}
	styled, err := ts.styled(ctx, in.Text, in.Format)
	if err != nil {
		return nil, SentOut{}, err
	}
	p, err := ts.t.resolveChat(ctx, in.Chat)
	if err != nil {
		return nil, SentOut{}, err
	}
	if err := ts.waitSendSlot(ctx); err != nil {
		return nil, SentOut{}, err
	}
	b := ts.builder(p, in.ReplyTo, in.Silent)
	upd, err := b.StyledText(ctx, styled)
	if err != nil {
		return nil, SentOut{}, friendly(err)
	}
	out := SentOut{ChatID: int64(p.TDLibPeerID()), ChatName: p.VisibleName(), MessageIDs: sentIDs(upd)}
	ts.audit.Write("send_message", map[string]any{
		"chat_id": out.ChatID, "chat": out.ChatName, "message_ids": out.MessageIDs,
		"reply_to": in.ReplyTo, "text": in.Text, "client": clientName(req),
	})
	return nil, out, nil
}

func (ts *toolset) builder(p peers.Peer, replyTo int, silent bool) *message.Builder {
	b := &ts.t.sender.To(p.InputPeer()).Builder
	if replyTo > 0 {
		b = b.Reply(replyTo)
	}
	if silent {
		b = b.Silent()
	}
	return b
}

func (ts *toolset) styled(ctx context.Context, text, format string) (styling.StyledTextOption, error) {
	switch format {
	case "", "plain":
		return styling.Plain(text), nil
	case "html":
		// The resolver turns <a href="tg://user?id=…"> into a real mention.
		return html.String(func(id int64) (tg.InputUserClass, error) {
			u, err := ts.t.peers.ResolveUserID(ctx, id)
			if err != nil {
				return nil, err
			}
			return u.InputUser(), nil
		}, text), nil
	default:
		return styling.StyledTextOption{}, fmt.Errorf("format must be plain or html")
	}
}

func (ts *toolset) waitSendSlot(ctx context.Context) error {
	r := ts.sendLimit.Reserve()
	if d := r.Delay(); d > 30*time.Second {
		r.Cancel()
		return fmt.Errorf("send rate limit: too many sends queued, retry in %s", d.Round(time.Second))
	} else if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			r.Cancel()
			return ctx.Err()
		}
	}
	return nil
}

// sentIDs pulls the new message IDs out of a send result.
func sentIDs(u tg.UpdatesClass) []int {
	ids := []int{}
	switch u := u.(type) {
	case *tg.UpdateShortSentMessage:
		ids = append(ids, u.ID)
	case *tg.Updates:
		for _, x := range u.Updates {
			switch x := x.(type) {
			case *tg.UpdateMessageID:
				ids = append(ids, x.ID)
			}
		}
		if len(ids) == 0 {
			for _, x := range u.Updates {
				switch x := x.(type) {
				case *tg.UpdateNewMessage:
					ids = append(ids, x.Message.GetID())
				case *tg.UpdateNewChannelMessage:
					ids = append(ids, x.Message.GetID())
				}
			}
		}
	}
	slices.Sort(ids)
	return ids
}

func clientName(req *mcp.CallToolRequest) string {
	if req == nil || req.Session == nil {
		return ""
	}
	if p := req.Session.InitializeParams(); p != nil && p.ClientInfo != nil {
		return p.ClientInfo.Name
	}
	return ""
}

// ---- send_file ------------------------------------------------------------

type SendFileIn struct {
	Chat       string   `json:"chat"`
	Paths      []string `json:"paths" jsonschema:"1-10 local file paths; more than one is sent as an album"`
	Caption    string   `json:"caption,omitempty" jsonschema:"shown under the first file (max 1024 characters)"`
	Format     string   `json:"format,omitempty" jsonschema:"caption format: plain (default) or html"`
	ReplyTo    int      `json:"reply_to,omitempty"`
	AsDocument bool     `json:"as_document,omitempty" jsonschema:"send images as files (uncompressed) instead of photos"`
	Silent     bool     `json:"silent,omitempty"`
}

var photoMIME = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}

const maxPhotoSize = 10 << 20

func (ts *toolset) sendFile(ctx context.Context, req *mcp.CallToolRequest, in SendFileIn) (*mcp.CallToolResult, SentOut, error) {
	if err := ts.ready(ctx); err != nil {
		return nil, SentOut{}, err
	}
	if len(in.Paths) == 0 || len(in.Paths) > 10 {
		return nil, SentOut{}, errors.New("paths must contain 1 to 10 files")
	}
	if n := utf8.RuneCountInString(in.Caption); n > 1024 {
		return nil, SentOut{}, fmt.Errorf("caption is %d characters; the limit is 1024", n)
	}

	type item struct {
		path, name, mime string
		size             int64
	}
	items := make([]item, 0, len(in.Paths))
	allPhotos := !in.AsDocument
	for _, raw := range in.Paths {
		real, size, err := ts.files.readable(raw)
		if err != nil {
			return nil, SentOut{}, err
		}
		it := item{path: real, name: filepath.Base(raw), size: size, mime: detectMIME(real)}
		allPhotos = allPhotos && photoMIME[it.mime] && size <= maxPhotoSize
		items = append(items, it)
	}
	p, err := ts.t.resolveChat(ctx, in.Chat)
	if err != nil {
		return nil, SentOut{}, err
	}
	var caption []styling.StyledTextOption
	if in.Caption != "" {
		c, err := ts.styled(ctx, in.Caption, in.Format)
		if err != nil {
			return nil, SentOut{}, err
		}
		caption = []styling.StyledTextOption{c}
	}

	// Telegram albums can't mix photos with documents, so it's all photos
	// (only when every file qualifies) or all documents.
	up := uploader.NewUploader(ts.t.api)
	opts := make([]message.MultiMediaOption, 0, len(items))
	for i, it := range items {
		f, err := up.FromPath(ctx, it.path)
		if err != nil {
			return nil, SentOut{}, fmt.Errorf("upload %s: %w", it.name, friendly(err))
		}
		var capt []styling.StyledTextOption
		if i == 0 {
			capt = caption
		}
		if allPhotos {
			opts = append(opts, message.UploadedPhoto(f, capt...))
		} else {
			opts = append(opts, message.UploadedDocument(f, capt...).Filename(it.name).MIME(it.mime).ForceFile(true))
		}
	}

	if err := ts.waitSendSlot(ctx); err != nil {
		return nil, SentOut{}, err
	}
	b := ts.builder(p, in.ReplyTo, in.Silent)
	var upd tg.UpdatesClass
	if len(opts) == 1 {
		upd, err = b.Media(ctx, opts[0])
	} else {
		upd, err = b.Album(ctx, opts[0], opts[1:]...)
	}
	if err != nil {
		return nil, SentOut{}, friendly(err)
	}
	out := SentOut{ChatID: int64(p.TDLibPeerID()), ChatName: p.VisibleName(), MessageIDs: sentIDs(upd)}
	files := make([]string, len(items))
	for i, it := range items {
		files[i] = it.path
	}
	ts.audit.Write("send_file", map[string]any{
		"chat_id": out.ChatID, "chat": out.ChatName, "message_ids": out.MessageIDs,
		"files": files, "caption": in.Caption, "reply_to": in.ReplyTo, "client": clientName(req),
	})
	return nil, out, nil
}

func detectMIME(path string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); t != "" {
		t, _, _ = strings.Cut(t, ";")
		return t
	}
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	t, _, _ := strings.Cut(http.DetectContentType(buf[:n]), ";")
	return t
}

// ---- send_reaction --------------------------------------------------------

type ReactIn struct {
	Chat      string `json:"chat"`
	MessageID int    `json:"message_id"`
	Emoji     string `json:"emoji" jsonschema:"a standard reaction emoji such as 👍 ❤ 🔥 🎉 😁; empty clears your reaction"`
}

type OKOut struct {
	OK bool `json:"ok"`
}

func (ts *toolset) sendReaction(ctx context.Context, req *mcp.CallToolRequest, in ReactIn) (*mcp.CallToolResult, OKOut, error) {
	if err := ts.ready(ctx); err != nil {
		return nil, OKOut{}, err
	}
	p, err := ts.t.resolveChat(ctx, in.Chat)
	if err != nil {
		return nil, OKOut{}, err
	}
	if err := ts.waitSendSlot(ctx); err != nil {
		return nil, OKOut{}, err
	}
	var reactions []tg.ReactionClass
	if in.Emoji != "" {
		reactions = append(reactions, &tg.ReactionEmoji{Emoticon: in.Emoji})
	}
	if _, err := ts.t.sender.To(p.InputPeer()).Reaction(ctx, in.MessageID, reactions...); err != nil {
		return nil, OKOut{}, friendly(err)
	}
	ts.audit.Write("send_reaction", map[string]any{
		"chat_id": int64(p.TDLibPeerID()), "chat": p.VisibleName(), "message_id": in.MessageID,
		"emoji": in.Emoji, "client": clientName(req),
	})
	return nil, OKOut{OK: true}, nil
}

// ---- mark_read ------------------------------------------------------------

type MarkReadIn struct {
	Chat  string `json:"chat"`
	MaxID int    `json:"max_id,omitempty" jsonschema:"mark read up to and including this message id; omit for everything"`
}

func (ts *toolset) markRead(ctx context.Context, _ *mcp.CallToolRequest, in MarkReadIn) (*mcp.CallToolResult, OKOut, error) {
	if err := ts.ready(ctx); err != nil {
		return nil, OKOut{}, err
	}
	p, err := ts.t.resolveChat(ctx, in.Chat)
	if err != nil {
		return nil, OKOut{}, err
	}
	if ch, ok := p.InputPeer().(*tg.InputPeerChannel); ok {
		_, err = ts.t.api.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash}, MaxID: in.MaxID,
		})
	} else {
		_, err = ts.t.api.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{Peer: p.InputPeer(), MaxID: in.MaxID})
	}
	if err != nil {
		return nil, OKOut{}, friendly(err)
	}
	return nil, OKOut{OK: true}, nil
}
