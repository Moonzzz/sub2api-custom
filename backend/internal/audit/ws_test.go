package audit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestWebSocketClientRawAndUpstreamTap(t *testing.T) {
	s := newTestStore(t, nil)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		for {
			typ, msg, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, append([]byte("echo:"), msg...)); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/v1/responses", WrapHandler(func(c *gin.Context) { c.Next() }, nil), func(c *gin.Context) {
		client, err := coderws.Accept(c.Writer, c.Request, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			return
		}
		defer func() { _ = client.CloseNow() }()
		ctx := c.Request.Context()
		up, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(upstream.URL, "http")+"/ws", &coderws.DialOptions{
			HTTPClient:      WrapClient(http.DefaultClient, AttemptInfo{AccountID: 77, Via: "test_ws"}),
			HTTPHeader:      http.Header{"Authorization": {"Bearer " + upstreamSecret}},
			CompressionMode: coderws.CompressionContextTakeover,
		})
		if err != nil {
			_ = client.Close(coderws.StatusInternalError, "dial")
			return
		}
		tap := NewWSTap(ctx)
		defer func() { _ = up.CloseNow(); tap.Closed("") }()
		for i := 0; i < 3; i++ {
			typ, msg, err := client.Read(ctx)
			if err != nil {
				return
			}
			if err := up.Write(ctx, typ, msg); err != nil {
				return
			}
			tap.Sent(ctx, WSOpText, msg)
			// 模拟连接池常驻读循环：不带归档器的 context 读取，仍归入绑定的请求。
			ut, um, err := up.Read(context.Background())
			if err != nil {
				return
			}
			tap.Received(context.Background(), WSOpText, um)
			if err := client.Write(ctx, ut, um); err != nil {
				return
			}
		}
		_ = client.Close(coderws.StatusNormalClosure, "bye")
	})
	gw := httptest.NewServer(r)
	defer gw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(gw.URL, "http")+"/v1/responses", &coderws.DialOptions{
		HTTPHeader:      http.Header{"Authorization": {"Bearer " + clientSecret}, "Session_id": {"ws-sess"}},
		CompressionMode: coderws.CompressionContextTakeover,
	})
	require.NoError(t, err)
	var sent []string
	for i := 0; i < 3; i++ {
		msg := fmt.Sprintf(`{"type":"response.create","i":%d,"pad":"%s"}`, i, strings.Repeat("abcdefgh", 200))
		sent = append(sent, msg)
		require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(msg)))
		_, echo, err := conn.Read(ctx)
		require.NoError(t, err)
		require.Equal(t, "echo:"+msg, string(echo))
	}
	_, _, err = conn.Read(ctx)
	require.Error(t, err) // 服务端关闭
	_ = conn.CloseNow()

	require.Eventually(t, func() bool { return s.stats.Requests.Load() == 1 && s.stats.RawBytes.Load() > 0 }, 5*time.Second, 20*time.Millisecond)
	ref := onlyRef(t, s, "ws-sess")
	av, err := s.ReadArchive(ref)
	require.NoError(t, err)

	cr := av.Files[0].Sections[0]
	require.Equal(t, "websocket", cr.Header.Transport)
	var clientTexts []string
	for _, m := range cr.Messages {
		if m.Op == "text" {
			clientTexts = append(clientTexts, m.Text)
			require.True(t, m.Compressed, "context takeover 压缩消息应被还原")
		}
	}
	require.Equal(t, sent, clientTexts)

	cw := av.Files[3].Sections[0]
	require.Equal(t, 101, cw.Header.Status)
	var echoes []string
	for _, m := range cw.Messages {
		if m.Op == "text" {
			echoes = append(echoes, m.Text)
		}
	}
	require.Len(t, echoes, 3)
	require.Equal(t, "echo:"+sent[2], echoes[2])
	require.Equal(t, "close", cw.Messages[len(cw.Messages)-1].Op)

	require.Equal(t, 101, av.Attempts[0].Status)
	require.EqualValues(t, 77, av.Attempts[0].AccountID)
	var upOut, upIn int
	for _, sec := range av.Files[1].Sections {
		upOut += len(sec.Messages)
	}
	for _, sec := range av.Files[2].Sections {
		upIn += len(sec.Messages)
	}
	require.Equal(t, 3, upOut)
	require.Equal(t, 3, upIn)
	all := decompressedAll(t, s, ref)
	require.NotContains(t, string(all), clientSecret)
	require.NotContains(t, string(all), upstreamSecret)
}
