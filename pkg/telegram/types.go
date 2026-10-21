// Package telegram is a dependency-free Telegram Bot API client plus the hub
// that mirrors usher's sessions into a forum supergroup, one topic per session.
// It speaks plain HTTPS+JSON over net/http — no third-party SDK.
package telegram

import (
	"encoding/json"
	"fmt"
)

// apiResponse is the envelope every Bot API method returns.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	// Parameters carries retry_after on a 429 (seconds to wait before retrying).
	Parameters struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// APIError is a non-ok Bot API response.
type APIError struct {
	Code        int
	Description string
	Method      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// Update is one item from getUpdates. Exactly one of the optional pointers is
// set for the update kinds usher cares about; the rest stay nil and are
// ignored.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// Message is a subset of the Bot API Message object. MessageThreadID is the
// forum topic (0 in the General topic / non-forum chats).
type Message struct {
	MessageID       int64  `json:"message_id"`
	MessageThreadID int64  `json:"message_thread_id,omitempty"`
	IsTopicMessage  bool   `json:"is_topic_message,omitempty"`
	From            *User  `json:"from,omitempty"`
	Chat            Chat   `json:"chat"`
	Text            string `json:"text,omitempty"`
}

// User is a subset of the Bot API User object.
type User struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username,omitempty"`
}

// Chat is a subset of the Bot API Chat object. Type is "private", "group",
// "supergroup", or "channel".
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// CallbackQuery arrives when a user taps an inline-keyboard button. Data is the
// opaque payload usher set on the button (≤64 bytes per Bot API); Message is
// the message the keyboard was attached to (so we can recover its topic).
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// ForumTopic is the result of createForumTopic; MessageThreadID is the id used
// to address the topic in subsequent sendMessage calls.
type ForumTopic struct {
	MessageThreadID int64  `json:"message_thread_id"`
	Name            string `json:"name"`
}

// InlineKeyboardMarkup / InlineKeyboardButton model the reply_markup used for
// permission allow/deny prompts. Only the callback_data variant is used.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

// SendMessageParams are the fields usher sets on sendMessage. ChatID and Text
// are required; the rest are optional and omitted when zero.
type SendMessageParams struct {
	ChatID          int64                 `json:"chat_id"`
	Text            string                `json:"text"`
	MessageThreadID int64                 `json:"message_thread_id,omitempty"`
	ParseMode       string                `json:"parse_mode,omitempty"`
	ReplyMarkup     *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
	// DisableNotification sends the message silently (no sound/vibration on the
	// recipient's device). usher uses it for the streamed assistant-text mirror,
	// reserving an audible ping for turn completion and permission prompts.
	DisableNotification bool `json:"disable_notification,omitempty"`
}

// SendPhotoParams are the fields usher sets on a multipart sendPhoto. The photo
// bytes and filename are passed separately to Client.SendPhoto.
type SendPhotoParams struct {
	ChatID              int64
	MessageThreadID     int64
	Caption             string
	DisableNotification bool
}
