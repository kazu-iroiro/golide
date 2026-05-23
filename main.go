package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image/color"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os/exec"
	"runtime"
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

type LogEntry struct {
	Time    string `json:"time"`
	Comment string `json:"comment"`
}

type SettingsRequest struct {
	Room    string  `json:"room"`
	Size    int     `json:"size"`
	Speed   float64 `json:"speed"`
	Colors  string  `json:"colors"`
	Outline string  `json:"outline"`
}

type Game struct {
	comments        []Comment
	archive         []LogEntry
	mu              sync.Mutex
	mplusNormalFont font.Face
	fontTemplate    *opentype.Font
	wsConn          *websocket.Conn

	fontSize         int
	fontColors       []color.Color
	outlineColor     color.Color
	roomName         string
	baseSpeed        float64
	rawColorsString  string
	rawOutlineString string

	shouldQuit bool
}

func (g *Game) Update() error {
	g.mu.Lock()
	quit := g.shouldQuit
	g.mu.Unlock()

	if quit || ebiten.IsKeyPressed(ebiten.KeyEscape) {
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
	for {
		g.mu.Lock()
		currentRoom := g.roomName
		g.mu.Unlock()

		if currentRoom == "" {
			time.Sleep(1 * time.Second)
			continue
		}

		url := fmt.Sprintf("wss://bolide.digicre.net/api/v1/room/%s", currentRoom)

		log.Printf("Info: Trying to connect to WebSocket for room [%s]...\n", currentRoom)
		connectTime := time.Now()

		c, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			log.Printf("Error: WebSocket connection failed: %v\n", err)
		} else {
			log.Println("Info: Successfully connected to WebSocket!")
			g.mu.Lock()
			g.wsConn = c
			g.mu.Unlock()

			for {
				_, message, err := c.ReadMessage()
				if err != nil {
					log.Printf("Error: WebSocket disconnected or room moved: %v\n", err)
					break
				}

				var wsMsg WsMessage
				if err := json.Unmarshal(message, &wsMsg); err != nil {
					log.Println("Error: Failed to parse JSON:", err)
					continue
				}

				cleanComment := sanitizeComment(wsMsg.Comment)
				if strings.TrimSpace(cleanComment) == "" {
					continue
				}

				w, h := ebiten.WindowSize()

				g.mu.Lock()
				nowStr := time.Now().Format("2006-01-02 15:04:05")
				g.archive = append(g.archive, LogEntry{Time: nowStr, Comment: cleanComment})

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

		g.mu.Lock()
		roomChanged := g.roomName != currentRoom
		g.mu.Unlock()

		if roomChanged {
			log.Println("Info: Room name has been changed. Reconnecting...")
			time.Sleep(500 * time.Millisecond)
			continue
		}

		if elapsed < reconnectShortSpan {
			log.Printf("Error: WebSocket disconnected too quickly. Waiting for %v...\n", reconnectWaitTime)
			time.Sleep(reconnectWaitTime)
		} else {
			log.Println("Info: Starting reconnection...")
			time.Sleep(1 * time.Second)
		}
	}
}

// --- Webダッシュボード（GUI）関連の処理 ---

func startWebServer(g *Game, startPort int) int {
	mux := http.NewServeMux()

	// Static file server for SVG and other assets
	embeddedFS := http.FileServer(http.FS(embedFS))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			data, err := embedFS.ReadFile("dashboard.html")
			if err != nil {
				log.Printf("Error: Failed to read dashboard.html: %v\n", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			w.Write(data)
		} else {
			embeddedFS.ServeHTTP(w, r)
		}
	}))

	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()

		data := map[string]interface{}{
			"room":    g.roomName,
			"speed":   g.baseSpeed,
			"size":    g.fontSize,
			"colors":  g.rawColorsString,
			"outline": g.rawOutlineString,
			"archive": g.archive,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	})

	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req SettingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		g.mu.Lock()

		g.baseSpeed = req.Speed
		g.fontColors = parseHexColors(req.Colors)
		g.rawColorsString = req.Colors

		outStr := strings.ToLower(strings.TrimSpace(req.Outline))
		g.rawOutlineString = req.Outline
		if outStr == "none" {
			g.outlineColor = nil
		} else if outStr == "black" {
			g.outlineColor = color.Black
		} else if outStr == "white" {
			g.outlineColor = color.White
		} else {
			if c, err := parseHexColor(outStr); err == nil {
				g.outlineColor = c
			} else {
				g.outlineColor = nil
			}
		}

		if req.Size != g.fontSize {
			newFace, err := opentype.NewFace(g.fontTemplate, &opentype.FaceOptions{
				Size:    float64(req.Size),
				DPI:     72,
				Hinting: font.HintingFull,
			})
			if err == nil {
				g.mplusNormalFont = newFace
				g.fontSize = req.Size
			} else {
				log.Printf("Error: Failed to change font size: %v\n", err)
			}
		}

		oldRoom := g.roomName
		g.roomName = req.Room
		if oldRoom != req.Room {
			log.Printf("Info: Room settings have been changed [%s -> %s].\n", oldRoom, req.Room)
			if g.wsConn != nil {
				g.wsConn.Close()
			}
		}

		g.mu.Unlock()

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		g.mu.Lock()
		if req.Action == "disconnect" {
			oldRoom := g.roomName
			g.roomName = ""
			if oldRoom != "" && g.wsConn != nil {
				log.Println("Info: Transitioning to idle state.")
				g.wsConn.Close()
			}
		} else if req.Action == "quit" {
			log.Println("Info: Quitting the application.")
			g.shouldQuit = true
		}
		g.mu.Unlock()

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	mux.HandleFunc("/export", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()

		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=\"comments_archive.csv\"")

		// UTF-8 BOMを出力
		w.Write([]byte{0xEF, 0xBB, 0xBF})

		fmt.Fprintln(w, "Time,Comment")
		for _, entry := range g.archive {
			escapedComment := strings.ReplaceAll(entry.Comment, "\"", "\"\"")
			fmt.Fprintf(w, "%s,\"%s\"\n", entry.Time, escapedComment)
		}
	})

	var listener net.Listener
	var err error
	actualPort := startPort

	for i := 0; i < 100; i++ {
		addr := fmt.Sprintf("127.0.0.1:%d", actualPort)
		listener, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		log.Printf("port: %d is in use. Trying the next port.", actualPort)
		actualPort++
	}

	if err != nil {
		log.Fatalf("Not found available ports: %v", err)
	}

	log.Printf("Web UI is Running: http://127.0.0.1:%d\n", actualPort)

	go func() {
		if err := http.Serve(listener, mux); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start HTTP server: %v", err)
		}
	}()

	return actualPort
}

// --- ユーティリティ・パース関連 ---
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
		}
	}

	if len(colors) == 0 {
		colors = append(colors, color.RGBA{255, 255, 255, 255})
	}

	return colors
}

func openAppWindow(url string) {
	var err error

	switch runtime.GOOS {
	case "windows":
		err = exec.Command("cmd", "/c", "start", "msedge", "--app="+url).Start()
		if err != nil {
			err = exec.Command("cmd", "/c", "start", "chrome", "--app="+url).Start()
		}
	case "darwin":
		err = exec.Command("open", "-n", "-a", "Google Chrome", "--args", "--app="+url).Start()
	case "linux":
		err = exec.Command("google-chrome", "--app="+url).Start()
	}

	if err != nil {
		log.Printf("Failed to open application window. Opening %s in the default browser.", url)
		switch runtime.GOOS {
		case "linux":
			exec.Command("xdg-open", url).Start()
		case "windows":
			exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
		case "darwin":
			exec.Command("open", url).Start()
		}
	}
}

func main() {
	roomPtr := flag.String("room", "", "接続するルーム名")
	sizePtr := flag.Int("size", 48, "フォントの大きさ")
	speedPtr := flag.Float64("speed", 4.0, "文字が進む基本速度")
	colorsPtr := flag.String("colors", "#FFFFFF,#FF0000,#FFFF00,#00FF00,#00FFFF,#FF00FF", "文字色")
	outlinePtr := flag.String("outline", "black", "文字の縁取り色 (none, black, white, または16進数)")
	webPortPtr := flag.Int("port", 8080, "Web UIのポート番号")

	flag.Parse()

	rand.Seed(time.Now().UnixNano())

	fontColors := parseHexColors(*colorsPtr)

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
			outlineColor = nil
		}
	}

	fontBytes, err := embedFS.ReadFile("MPLUS1p-Bold.ttf")
	if err != nil {
		log.Fatalf("Error: Failed to read font file: %v", err)
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
		mplusNormalFont:  mplusNormalFont,
		fontTemplate:     tt,
		fontSize:         *sizePtr,
		fontColors:       fontColors,
		outlineColor:     outlineColor,
		roomName:         *roomPtr,
		baseSpeed:        *speedPtr,
		rawColorsString:  *colorsPtr,
		rawOutlineString: *outlinePtr,
		archive:          make([]LogEntry, 0),
		shouldQuit:       false,
	}

	go game.listenWebSocket()

	actualPort := startWebServer(game, *webPortPtr)

	if *roomPtr == "" {
		go func() {
			time.Sleep(500 * time.Millisecond)
			openAppWindow(fmt.Sprintf("http://127.0.0.1:%d", actualPort))
		}()
	}

	ebiten.SetWindowTitle("Comment Viewer")
	ebiten.SetWindowDecorated(false)
	ebiten.SetWindowFloating(true)
	ebiten.SetWindowMousePassthrough(true)

	sw, sh := ebiten.Monitor().Size()
	ebiten.SetWindowSize(sw, sh-1)
	ebiten.SetWindowPosition(0, 0)

	options := &ebiten.RunGameOptions{
		ScreenTransparent: true,
	}

	if err := ebiten.RunGameWithOptions(game, options); err != nil {
		log.Fatal(err)
	}
}
