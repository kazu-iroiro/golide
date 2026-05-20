package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image/color"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/text"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

const (
	reconnectShortSpan = 5 * time.Second
	reconnectWaitTime  = 10 * time.Second
	maxCommentLength   = 100
)

type WsMessage struct {
	Comment    string `json:"comment"`
	IsQuestion bool   `json:"is_question"`
}

type Comment struct {
	Text  string
	X     float64
	Y     float64
	Speed float64
	Color color.Color
}

type Game struct {
	comments        []Comment
	mu              sync.Mutex
	mplusNormalFont font.Face

	fontSize     int
	fontColors   []color.Color
	outlineColor color.Color // ★ 縁取りの色（nil の場合は描画しない）
	roomName     string
	baseSpeed    float64
}

func (g *Game) Update() error {
	if ebiten.IsKeyPressed(ebiten.KeyEscape) {
		return ebiten.Termination
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	activeComments := g.comments[:0]
	for _, c := range g.comments {
		c.X -= c.Speed

		textWidth := float64(len([]rune(c.Text)) * g.fontSize)
		if c.X+textWidth > 0 {
			activeComments = append(activeComments, c)
		}
	}
	g.comments = activeComments

	return nil
}

func (g *Game) Draw(screen *ebiten.Image) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for _, c := range g.comments {
		// ★ 縁取りが設定されている場合は上下左右斜めにずらして描画
		if g.outlineColor != nil {
			x, y := int(c.X), int(c.Y)
			text.Draw(screen, c.Text, g.mplusNormalFont, x-1, y-1, g.outlineColor)
			text.Draw(screen, c.Text, g.mplusNormalFont, x+1, y-1, g.outlineColor)
			text.Draw(screen, c.Text, g.mplusNormalFont, x-1, y+1, g.outlineColor)
			text.Draw(screen, c.Text, g.mplusNormalFont, x+1, y+1, g.outlineColor)
			text.Draw(screen, c.Text, g.mplusNormalFont, x, y-1, g.outlineColor)
			text.Draw(screen, c.Text, g.mplusNormalFont, x, y+1, g.outlineColor)
			text.Draw(screen, c.Text, g.mplusNormalFont, x-1, y, g.outlineColor)
			text.Draw(screen, c.Text, g.mplusNormalFont, x+1, y, g.outlineColor)
		}

		// メインの文字を描画
		text.Draw(screen, c.Text, g.mplusNormalFont, int(c.X), int(c.Y), c.Color)
	}
}

func (g *Game) Layout(outsideWidth, outsideHeight int) (int, int) {
	return outsideWidth, outsideHeight
}

func sanitizeComment(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\t", " ")

	if utf8.RuneCountInString(s) > maxCommentLength {
		runes := []rune(s)
		s = string(runes[:maxCommentLength]) + "..."
	}
	return s
}

func (g *Game) listenWebSocket() {
	url := fmt.Sprintf("wss://bolide.digicre.net/api/v1/room/%s", g.roomName)

	for {
		log.Printf("ルーム [%s] の WebSocketに接続を試みています...\n", g.roomName)
		connectTime := time.Now()

		c, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			log.Printf("WebSocket接続エラー: %v\n", err)
		} else {
			log.Println("WebSocketに接続しました！")

			for {
				_, message, err := c.ReadMessage()
				if err != nil {
					log.Printf("WebSocket切断: %v\n", err)
					break
				}

				var wsMsg WsMessage
				if err := json.Unmarshal(message, &wsMsg); err != nil {
					log.Println("JSONパースエラー:", err)
					continue
				}

				cleanComment := sanitizeComment(wsMsg.Comment)
				if strings.TrimSpace(cleanComment) == "" {
					continue
				}

				w, h := ebiten.WindowSize()
				g.mu.Lock()

				randomOffset := rand.Float64() * 2.0
				actualSpeed := g.baseSpeed + randomOffset

				selectedColor := g.fontColors[rand.Intn(len(g.fontColors))]

				newComment := Comment{
					Text:  cleanComment,
					X:     float64(w),
					Y:     float64(rand.Intn(h-g.fontSize*2) + g.fontSize),
					Speed: actualSpeed,
					Color: selectedColor,
				}
				g.comments = append(g.comments, newComment)
				g.mu.Unlock()
			}
			c.Close()
		}

		elapsed := time.Since(connectTime)
		if elapsed < reconnectShortSpan {
			log.Printf("エラー/切断のスパンが短いため、%v 間待機します...\n", reconnectWaitTime)
			time.Sleep(reconnectWaitTime)
		} else {
			log.Println("再接続を開始します...")
			time.Sleep(1 * time.Second)
		}
	}
}

func parseHexColor(s string) (c color.RGBA, err error) {
	c.A = 0xff
	if s[0] == '#' {
		s = s[1:]
	}

	var r, g, b uint8
	switch len(s) {
	case 6:
		_, err = fmt.Sscanf(s, "%02x%02x%02x", &r, &g, &b)
		c.R = r
		c.G = g
		c.B = b
	case 3:
		_, err = fmt.Sscanf(s, "%1x%1x%1x", &r, &g, &b)
		c.R = r * 17
		c.G = g * 17
		c.B = b * 17
	default:
		err = fmt.Errorf("invalid color format")
	}
	return
}

func parseHexColors(s string) []color.Color {
	parts := strings.Split(s, ",")
	var colors []color.Color

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		c, err := parseHexColor(p)
		if err == nil {
			colors = append(colors, c)
		} else {
			log.Printf("警告: %s は不正なカラーコードのためスキップします\n", p)
		}
	}

	if len(colors) == 0 {
		colors = append(colors, color.RGBA{255, 255, 255, 255})
	}

	return colors
}

func main() {
	roomPtr := flag.String("room", "test77", "接続するルーム名")
	sizePtr := flag.Int("size", 24, "フォントの大きさ")
	speedPtr := flag.Float64("speed", 3.0, "文字が進む基本速度")
	colorsPtr := flag.String("colors", "#FFFFFF,#FF0000,#FFFF00,#00FF00,#00FFFF,#FF00FF", "文字色 (カンマ区切りで複数指定可能)")

	// ★ デフォルトを "none" に変更
	outlinePtr := flag.String("outline", "none", "文字の縁取り色 (none, black, white, または16進数)")

	flag.Parse()

	rand.Seed(time.Now().UnixNano())

	fontColors := parseHexColors(*colorsPtr)

	// ★ 縁取り色の判定ロジック（HEX指定にも対応）
	var outlineColor color.Color
	outStr := strings.ToLower(strings.TrimSpace(*outlinePtr))
	if outStr == "none" {
		outlineColor = nil
	} else if outStr == "black" {
		outlineColor = color.Black
	} else if outStr == "white" {
		outlineColor = color.White
	} else {
		c, err := parseHexColor(outStr)
		if err == nil {
			outlineColor = c
		} else {
			log.Printf("無効な outline 指定 (%s)。縁取りなし(none)を使用します。\n", *outlinePtr)
			outlineColor = nil
		}
	}

	fontBytes, err := os.ReadFile("MPLUS1p-Bold.ttf")
	if err != nil {
		log.Fatalf("フォントファイルの読み込みに失敗しました: %v", err)
	}
	tt, err := opentype.Parse(fontBytes)
	if err != nil {
		log.Fatal(err)
	}

	mplusNormalFont, err := opentype.NewFace(tt, &opentype.FaceOptions{
		Size:    float64(*sizePtr),
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		log.Fatal(err)
	}

	game := &Game{
		mplusNormalFont: mplusNormalFont,
		fontSize:        *sizePtr,
		fontColors:      fontColors,
		outlineColor:    outlineColor, // パースした縁取り色をセット
		roomName:        *roomPtr,
		baseSpeed:       *speedPtr,
	}

	go game.listenWebSocket()

	ebiten.SetWindowTitle("Comment Viewer")
	ebiten.SetWindowDecorated(false)
	ebiten.SetWindowFloating(true)
	ebiten.SetWindowMousePassthrough(true)

	sw, sh := ebiten.Monitor().Size()
	ebiten.SetWindowSize(sw, sh)
	ebiten.SetWindowPosition(0, 0)

	options := &ebiten.RunGameOptions{
		ScreenTransparent: true,
	}

	if err := ebiten.RunGameWithOptions(game, options); err != nil {
		log.Fatal(err)
	}
}
