package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// larkAPI is the slice of the Lark Open API the hub uses; a fake stands in
// for it in tests.
type larkAPI interface {
	// SendCard posts a card as a chat message (the thread root) and returns
	// its message id.
	SendCard(ctx context.Context, chatID, cardJSON string) (string, error)
	// UpdateCard replaces a sent card's content in place.
	UpdateCard(ctx context.Context, messageID, cardJSON string) error
	// ReplyText replies to rootID inside its thread and returns the thread id
	// (may be "" when the server omits it).
	ReplyText(ctx context.Context, rootID, text string) (string, error)
	// ReplyCard replies with an interactive card inside rootID's thread.
	ReplyCard(ctx context.Context, rootID, cardJSON string) (string, error)
	// ReplyPost replies with a rich-text (post) message inside rootID's thread.
	ReplyPost(ctx context.Context, rootID, postJSON string) (string, error)
	// UploadImage uploads a message image and returns its image_key.
	UploadImage(ctx context.Context, data []byte) (string, error)
	// ReplyImage replies with an uploaded image inside rootID's thread.
	ReplyImage(ctx context.Context, rootID, imageKey string) (string, error)
	// React adds an emoji reaction to a message.
	React(ctx context.Context, messageID, emojiType string) error
}

// larkClient implements larkAPI on the official SDK client.
type larkClient struct {
	c *lark.Client
}

func newLarkClient(appID, appSecret, domain string) *larkClient {
	return &larkClient{c: lark.NewClient(appID, appSecret, lark.WithOpenBaseUrl(domain))}
}

// textContent renders the im "text" message content JSON.
func textContent(text string) string {
	data, _ := json.Marshal(map[string]string{"text": text})
	return string(data)
}

func (l *larkClient) SendCard(ctx context.Context, chatID, cardJSON string) (string, error) {
	resp, err := l.c.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(larkim.MsgTypeInteractive).
			Content(cardJSON).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("send card", resp.Code, resp.Msg)
	}
	return deref(resp.Data.MessageId), nil
}

func (l *larkClient) UpdateCard(ctx context.Context, messageID, cardJSON string) error {
	resp, err := l.c.Im.V1.Message.Patch(ctx, larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(cardJSON).
			Build()).
		Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return apiErr("update card", resp.Code, resp.Msg)
	}
	return nil
}

// reply sends one threaded reply of any msg type and returns the thread id.
func (l *larkClient) reply(ctx context.Context, rootID, msgType, content string) (string, error) {
	resp, err := l.c.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(rootID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType(msgType).
			Content(content).
			ReplyInThread(true).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("reply", resp.Code, resp.Msg)
	}
	return deref(resp.Data.ThreadId), nil
}

func (l *larkClient) ReplyText(ctx context.Context, rootID, text string) (string, error) {
	return l.reply(ctx, rootID, larkim.MsgTypeText, textContent(text))
}

func (l *larkClient) ReplyCard(ctx context.Context, rootID, cardJSON string) (string, error) {
	return l.reply(ctx, rootID, larkim.MsgTypeInteractive, cardJSON)
}

func (l *larkClient) ReplyPost(ctx context.Context, rootID, postJSON string) (string, error) {
	return l.reply(ctx, rootID, larkim.MsgTypePost, postJSON)
}

func (l *larkClient) UploadImage(ctx context.Context, data []byte) (string, error) {
	resp, err := l.c.Im.V1.Image.Create(ctx, larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().
			ImageType(larkim.CreateImageImageTypeMessage).
			Image(bytes.NewReader(data)).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("upload image", resp.Code, resp.Msg)
	}
	return deref(resp.Data.ImageKey), nil
}

func (l *larkClient) ReplyImage(ctx context.Context, rootID, imageKey string) (string, error) {
	data, _ := json.Marshal(map[string]string{"image_key": imageKey})
	return l.reply(ctx, rootID, larkim.MsgTypeImage, string(data))
}

func (l *larkClient) React(ctx context.Context, messageID, emojiType string) error {
	resp, err := l.c.Im.V1.MessageReaction.Create(ctx, larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emojiType).Build()).
			Build()).
		Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return apiErr("react", resp.Code, resp.Msg)
	}
	return nil
}

func apiErr(op string, code int, msg string) error {
	return fmt.Errorf("lark %s: code %d: %s", op, code, msg)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
