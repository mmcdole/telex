package main

import (
	"strings"
	"time"

	"github.com/gotd/td/constant"
	"github.com/gotd/td/tg"
)

// Message is the shape every tool returns for a Telegram message.
type Message struct {
	ID       int    `json:"id"`
	Date     string `json:"date"`
	ChatID   int64  `json:"chat_id,omitempty" jsonschema:"marked chat ID, pass back as chat"`
	ChatName string `json:"chat_name,omitempty"`
	FromID   int64  `json:"from_id,omitempty"`
	FromName string `json:"from_name,omitempty"`
	Out      bool   `json:"out,omitempty" jsonschema:"sent by this account"`
	Text     string `json:"text"`
	Media    *Media `json:"media,omitempty"`
	ReplyTo  int    `json:"reply_to,omitempty"`
	Edited   bool   `json:"edited,omitempty"`
	Forward  string `json:"forwarded_from,omitempty"`
	Service  string `json:"service,omitempty" jsonschema:"set for service messages (joins, pins...) to the action type"`
}

// Media summarizes an attachment; download_media fetches the bytes.
type Media struct {
	Kind     string `json:"kind" jsonschema:"photo, video, voice, audio, sticker, gif, document, or another Telegram media type"`
	FileName string `json:"file_name,omitempty"`
	MIME     string `json:"mime,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Duration int    `json:"duration,omitempty"`
}

// entities is the users and chats that came alongside a response or update,
// used to put names on IDs.
type entities struct {
	users    map[int64]*tg.User
	chats    map[int64]*tg.Chat
	channels map[int64]*tg.Channel
}

func newEntities(users []tg.UserClass, chats []tg.ChatClass) entities {
	e := entities{users: map[int64]*tg.User{}, chats: map[int64]*tg.Chat{}, channels: map[int64]*tg.Channel{}}
	tg.UserClassArray(users).FillNotEmptyMap(e.users)
	tg.ChatClassArray(chats).FillChatMap(e.chats)
	tg.ChatClassArray(chats).FillChannelMap(e.channels)
	return e
}

func entitiesFromUpdate(u tg.Entities) entities {
	return entities{users: u.Users, chats: u.Chats, channels: u.Channels}
}

func (e entities) name(p tg.PeerClass) string {
	switch p := p.(type) {
	case *tg.PeerUser:
		if u, ok := e.users[p.UserID]; ok {
			return userName(u)
		}
	case *tg.PeerChat:
		if c, ok := e.chats[p.ChatID]; ok {
			return c.Title
		}
	case *tg.PeerChannel:
		if c, ok := e.channels[p.ChannelID]; ok {
			return c.Title
		}
	}
	return ""
}

// kind reports user, bot, group, or channel for a peer.
func (e entities) kind(p tg.PeerClass) string {
	switch p := p.(type) {
	case *tg.PeerUser:
		if u, ok := e.users[p.UserID]; ok && u.Bot {
			return "bot"
		}
		return "user"
	case *tg.PeerChat:
		return "group"
	case *tg.PeerChannel:
		if c, ok := e.channels[p.ChannelID]; ok && c.Broadcast {
			return "channel"
		}
		return "group"
	}
	return "unknown"
}

func userName(u *tg.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" {
		name = u.Username
	}
	if u.Deleted && name == "" {
		name = "Deleted Account"
	}
	return name
}

// markedID is the Bot API / TDLib style ID: users positive, basic groups
// negative, channels and supergroups -100…. It is unambiguous across types,
// which a bare Telegram ID is not.
func markedID(p tg.PeerClass) int64 {
	var id constant.TDLibPeerID
	switch p := p.(type) {
	case *tg.PeerUser:
		id.User(p.UserID)
	case *tg.PeerChat:
		id.Chat(p.ChatID)
	case *tg.PeerChannel:
		id.Channel(p.ChannelID)
	default:
		return 0
	}
	return int64(id)
}

func unixISO(ts int) string {
	if ts == 0 {
		return ""
	}
	return time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
}

// convertMessage flattens a tg message. chat is the dialog it belongs to,
// withChat adds chat_id/chat_name (for results spanning several chats).
func convertMessage(m tg.MessageClass, e entities, withChat bool) (Message, bool) {
	switch m := m.(type) {
	case *tg.Message:
		out := Message{
			ID:     m.ID,
			Date:   unixISO(m.Date),
			Out:    m.Out,
			Text:   m.Message,
			Media:  convertMedia(m.Media),
			Edited: m.EditDate != 0 && !m.EditHide,
		}
		fillSender(&out, m.FromID, m.PeerID, m.Out, e)
		if r, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok {
			out.ReplyTo = r.ReplyToMsgID
		}
		if fwd, ok := m.GetFwdFrom(); ok {
			out.Forward = fwd.FromName
			if from, ok := fwd.GetFromID(); ok && out.Forward == "" {
				out.Forward = e.name(from)
			}
			if out.Forward == "" {
				out.Forward = "unknown"
			}
		}
		if withChat {
			out.ChatID, out.ChatName = markedID(m.PeerID), e.name(m.PeerID)
		}
		return out, true
	case *tg.MessageService:
		out := Message{
			ID:      m.ID,
			Date:    unixISO(m.Date),
			Out:     m.Out,
			Service: strings.TrimPrefix(m.Action.TypeName(), "messageAction"),
		}
		fillSender(&out, m.FromID, m.PeerID, m.Out, e)
		if withChat {
			out.ChatID, out.ChatName = markedID(m.PeerID), e.name(m.PeerID)
		}
		return out, true
	}
	return Message{}, false
}

func fillSender(out *Message, from, chat tg.PeerClass, isOut bool, e entities) {
	// In private chats the sender is omitted; it is the other user unless the
	// message is outgoing.
	if from == nil && !isOut {
		if _, private := chat.(*tg.PeerUser); private {
			from = chat
		} else if _, ch := chat.(*tg.PeerChannel); ch {
			from = chat // channel posts are authored by the channel
		}
	}
	if from != nil {
		out.FromID, out.FromName = markedID(from), e.name(from)
	}
}

func convertMedia(m tg.MessageMediaClass) *Media {
	switch m := m.(type) {
	case nil, *tg.MessageMediaEmpty:
		return nil
	case *tg.MessageMediaPhoto:
		return &Media{Kind: "photo"}
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return &Media{Kind: "document"}
		}
		md := &Media{Kind: "document", MIME: doc.MimeType, Size: doc.Size}
		for _, a := range doc.Attributes {
			switch a := a.(type) {
			case *tg.DocumentAttributeFilename:
				md.FileName = a.FileName
			case *tg.DocumentAttributeAudio:
				md.Kind, md.Duration = "audio", a.Duration
				if a.Voice {
					md.Kind = "voice"
				}
			case *tg.DocumentAttributeVideo:
				md.Kind, md.Duration = "video", int(a.Duration)
				if a.RoundMessage {
					md.Kind = "video_note"
				}
			case *tg.DocumentAttributeSticker:
				md.Kind = "sticker"
			case *tg.DocumentAttributeAnimated:
				md.Kind = "gif"
			}
		}
		return md
	default:
		return &Media{Kind: strings.TrimPrefix(m.TypeName(), "messageMedia")}
	}
}

// preview shortens text for chat listings.
func preview(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
