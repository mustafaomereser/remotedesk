package host

import (
	"context"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Conn, oturumun karşı tarafla konuştuğu kanaldır: WebSocket (tünel) ya da WebRTC veri
// kanalları (doğrudan bağlantı). Oturum kodu hangisi olduğunu bilmez.
type Conn interface {
	ReadJSON(ctx context.Context, v any) error
	WriteJSON(ctx context.Context, v any) error
	WriteBinary(ctx context.Context, b []byte) error
	Close(reason string)
	// Kind, arayüzde gösterilecek bağlantı türüdür: "tunnel" | "direct".
	Kind() string
}

type wsConn struct{ c *websocket.Conn }

func (w wsConn) ReadJSON(ctx context.Context, v any) error  { return wsjson.Read(ctx, w.c, v) }
func (w wsConn) WriteJSON(ctx context.Context, v any) error { return wsjson.Write(ctx, w.c, v) }
func (w wsConn) WriteBinary(ctx context.Context, b []byte) error {
	return w.c.Write(ctx, websocket.MessageBinary, b)
}
func (w wsConn) Close(reason string) { w.c.Close(websocket.StatusNormalClosure, reason) }
func (w wsConn) Kind() string        { return "tunnel" }
