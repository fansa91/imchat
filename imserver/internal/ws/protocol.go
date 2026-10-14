package ws

import "encoding/json"

// Envelope 是客户端与服务端之间所有 WebSocket 帧的统一格式。
//
// 客户端 -> 服务端 type:
//
//	send    {client_msg_id, conversation_id, msg_type, content, reply_to_seq?}
//	read    {conversation_id, seq}
//	typing  {conversation_id}
//	recall  {conversation_id, seq}                 撤回自己的消息（时间窗口内）
//	react   {conversation_id, seq, emoji, on?}     表情回应；on 省略为切换
//	delete  {conversation_id, seqs:[...]}          仅自己删除（最多 100 条）
//	ping    {}
//
// 服务端 -> 客户端 type:
//
//	send_ack  {message, duplicate}        发送成功回执（含服务端分配的 seq）
//	message   {message}                   新消息（包括自己其他设备发出的、系统消息）
//	read      {conversation_id, user_id, read_seq}
//	typing    {conversation_id, user_id}
//	recall    {conversation_id, seq, user_id, recalled_at, mod_seq, message}
//	reaction  {conversation_id, seq, user_id, emoji, on, reactions, mod_seq}
//	deleted   {conversation_id, seqs}     自己（任一设备）删除了这些消息
//	conversation_updated {conversation_id, reason}  群信息/成员变化，客户端刷新会话
//	conversation_removed {conversation_id, reason}  自己已不在该会话（退群/被移出）
//	ack       {op, ...}                   recall/react/delete 的请求回执（带请求 id）
//	pong      {}
//	error     {code, message}
type Envelope struct {
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"` // 客户端请求 ID，服务端在回复中原样返回
	Data json.RawMessage `json:"data,omitempty"`
}

type sendReq struct {
	ClientMsgID    string `json:"client_msg_id"`
	ConversationID uint64 `json:"conversation_id"`
	MsgType        int    `json:"msg_type"`
	Content        string `json:"content"`
	ReplyToSeq     uint64 `json:"reply_to_seq"`
}

type readReq struct {
	ConversationID uint64 `json:"conversation_id"`
	Seq            uint64 `json:"seq"`
}

type typingReq struct {
	ConversationID uint64 `json:"conversation_id"`
}

type recallReq struct {
	ConversationID uint64 `json:"conversation_id"`
	Seq            uint64 `json:"seq"`
}

type reactReq struct {
	ConversationID uint64 `json:"conversation_id"`
	Seq            uint64 `json:"seq"`
	Emoji          string `json:"emoji"`
	On             *bool  `json:"on"`
}

type deleteReq struct {
	ConversationID uint64   `json:"conversation_id"`
	Seqs           []uint64 `json:"seqs"`
}

// VoiceContent 语音消息（msg_type=3）的 content。
type VoiceContent struct {
	Key        string `json:"key"`
	Mime       string `json:"mime"`
	DurationMs int    `json:"duration_ms"`
	Size       int64  `json:"size"`
	Waveform   []int  `json:"waveform,omitempty"` // 可选：最多 64 个 0~100 的振幅，用于绘制波形
}

type errorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func encode(typ, id string, data any) []byte {
	raw, _ := json.Marshal(data)
	b, _ := json.Marshal(Envelope{Type: typ, ID: id, Data: raw})
	return b
}

// Encode 供 HTTP 接口构造推送帧。
func Encode(typ string, data any) []byte { return encode(typ, "", data) }
