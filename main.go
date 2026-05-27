package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	maxRecentEntries   = 1000 // メモリに保持する最新コメント数
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
	Display string  `json:"display"`
	WindowX int     `json:"windowX"` // OSに対するウィンドウの開始X座標
	WindowY int     `json:"windowY"` // OSに対するウィンドウの開始Y座標
}

type Game struct {
	comments        []Comment
	recentArchive   []LogEntry // メモリに保持する最新コメント
	archiveFile     *os.File   // アーカイブファイル
	archiveFilePath string     // アーカイブファイルのパス
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

	// ディスプレイ制御
	displayMode string
	windowX     int // Ebitengineの原点誤認を補正するためのOS座標
	windowY     int
	totalW      int // 全モニターを覆うピッタリの横幅
	maxH        int // 全モニターを覆うピッタリの高さ

	// ウィンドウ内部における論理的な描画領域（クリッピング）
	clipX, clipY float64
	clipW, clipH float64

	updateDisplay bool
	shouldQuit    bool
}

// アプリ起動時に1度だけ、全画面をピッタリ覆うウィンドウサイズを計算する
func (g *Game) initWindowSize() {
	mons := ebiten.AppendMonitors(nil)
	if len(mons) == 0 {
		g.totalW, g.maxH = 1920, 1080
		return
	}

	totalWidth := 0
	maxHeight := 0
	for _, m := range mons {
		w, h := m.Size()
		totalWidth += w
		if h > maxHeight {
			maxHeight = h
		}
	}
	g.totalW = totalWidth
	g.maxH = maxHeight
}

// 選択されたディスプレイに応じて、ウィンドウ内部の描画領域を論理的に切り替える
func (g *Game) applyLogicalClip() {
	g.mu.Lock()
	defer g.mu.Unlock()

	mons := ebiten.AppendMonitors(nil)
	if len(mons) == 0 {
		return
	}

	if g.displayMode == "all" {
		// 全てのディスプレイを横断
		g.clipX = 0
		g.clipY = 0
		g.clipW = float64(g.totalW)
		g.clipH = float64(g.maxH)
	} else {
		// 特定のディスプレイ（例: 0番目、1番目）
		var selIdx int
		fmt.Sscanf(g.displayMode, "%d", &selIdx)
		if selIdx < 0 || selIdx >= len(mons) {
			selIdx = 0
		}

		// 選択されたディスプレイまでの横幅を合計し、描画開始X座標(clipX)とする
		startX := 0
		for i := 0; i < selIdx; i++ {
			w, _ := mons[i].Size()
			startX += w
		}
		mw, mh := mons[selIdx].Size()

		g.clipX = float64(startX)
		g.clipY = 0
		g.clipW = float64(mw)
		g.clipH = float64(mh)
	}
}

// アーカイブファイルにコメントを追記
func (g *Game) appendArchive(entry LogEntry) error {
	if g.archiveFile == nil {
		return fmt.Errorf("archive file not open")
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = g.archiveFile.WriteString(string(data) + "\n")
	return err
}

// メモリ内の最新コメントを管理（上限を超えたら古い物を削除）
func (g *Game) addRecentArchive(entry LogEntry) {
	g.recentArchive = append(g.recentArchive, entry)
	if len(g.recentArchive) > maxRecentEntries {
		// 古い方から削除
		g.recentArchive = g.recentArchive[len(g.recentArchive)-maxRecentEntries:]
	}
}

func (g *Game) Update() error {
	g.mu.Lock()
	quit := g.shouldQuit
	updateDisp := g.updateDisplay
	if updateDisp {
		g.updateDisplay = false
	}
	g.mu.Unlock()

	if quit || ebiten.IsKeyPressed(ebiten.KeyEscape) {
		return ebiten.Termination
	}

	// UIからの変更検知時、OSウィンドウの配置と内部クリッピングを更新
	if updateDisp {
		ebiten.SetWindowPosition(g.windowX, g.windowY)
		g.applyLogicalClip()
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// 画面内に残っているコメントだけを保持（メモリ効率化）
	activeComments := g.comments[:0]
	for _, c := range g.comments {
		c.X -= c.Speed
		textWidth := float64(len([]rune(c.Text)) * g.fontSize)

		// ウィンドウ内部の論理的な左端（clipX）を越えるまで生存させる
		if c.X+textWidth > g.clipX {
			activeComments = append(activeComments, c)
		}
	}
	g.comments = activeComments
	return nil
}

func (g *Game) Draw(screen *ebiten.Image) {
	g.mu.Lock()
	defer g.mu.Unlock()

	clipRect := image.Rect(int(g.clipX), int(g.clipY), int(g.clipX+g.clipW), int(g.clipY+g.clipH))
	clippedScreen := screen.SubImage(clipRect).(*ebiten.Image)

	for _, c := range g.comments {
		if g.outlineColor != nil {
			x, y := int(c.X), int(c.Y)
			text.Draw(clippedScreen, c.Text, g.mplusNormalFont, x-1, y-1, g.outlineColor)
			text.Draw(clippedScreen, c.Text, g.mplusNormalFont, x+1, y-1, g.outlineColor)
			text.Draw(clippedScreen, c.Text, g.mplusNormalFont, x-1, y+1, g.outlineColor)
			text.Draw(clippedScreen, c.Text, g.mplusNormalFont, x+1, y+1, g.outlineColor)
			text.Draw(clippedScreen, c.Text, g.mplusNormalFont, x, y-1, g.outlineColor)
			text.Draw(clippedScreen, c.Text, g.mplusNormalFont, x, y+1, g.outlineColor)
			text.Draw(clippedScreen, c.Text, g.mplusNormalFont, x-1, y, g.outlineColor)
			text.Draw(clippedScreen, c.Text, g.mplusNormalFont, x+1, y, g.outlineColor)
		}
		text.Draw(clippedScreen, c.Text, g.mplusNormalFont, int(c.X), int(c.Y), c.Color)
	}
}

func (g *Game) Layout(outsideWidth, outsideHeight int) (int, int) {
	// ebiten側から算出したピッタリのサイズを維持する
	return g.totalW, g.maxH
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
		c, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			log.Printf("Error: %v\n", err)
		} else {
			g.mu.Lock()
			g.wsConn = c
			g.mu.Unlock()

			for {
				_, message, err := c.ReadMessage()
				if err != nil {
					break
				}
				var wsMsg WsMessage
				json.Unmarshal(message, &wsMsg)

				cleanComment := sanitizeComment(wsMsg.Comment)
				if strings.TrimSpace(cleanComment) == "" {
					continue
				}

				nowStr := time.Now().Format("2006-01-02 15:04:05")
				entry := LogEntry{Time: nowStr, Comment: cleanComment}

				// ファイルに追記（ロック外で実行）
				if err := g.appendArchive(entry); err != nil {
					log.Printf("Error writing archive: %v\n", err)
				}

				g.mu.Lock()
				// メモリには最新分だけ保持
				g.addRecentArchive(entry)

				yRange := int(g.clipH) - g.fontSize*2
				if yRange <= 0 {
					yRange = 1
				}

				newComment := Comment{
					Text:  cleanComment,
					X:     g.clipX + g.clipW, // 論理的な右端からスタート
					Y:     g.clipY + float64(rand.Intn(yRange)) + float64(g.fontSize),
					Speed: g.baseSpeed + rand.Float64()*2.0,
					Color: g.fontColors[rand.Intn(len(g.fontColors))],
				}
				g.comments = append(g.comments, newComment)
				g.mu.Unlock()
			}
			c.Close()
		}
		time.Sleep(1 * time.Second)
	}
}

func startWebServer(g *Game, startPort int) int {
	mux := http.NewServeMux()

	embeddedFS := http.FileServer(http.FS(embedFS))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			data, err := embedFS.ReadFile("dashboard.html")
			if err != nil {
				http.Error(w, "Not Found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(data)
			return
		}
		embeddedFS.ServeHTTP(w, r)
	})

	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()

		mons := ebiten.AppendMonitors(nil)
		type MonitorInfo struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		infos := make([]MonitorInfo, len(mons))
		for i, m := range mons {
			mw, mh := m.Size()
			infos[i] = MonitorInfo{fmt.Sprintf("%d", i), fmt.Sprintf("Display %d (%dx%d)", i+1, mw, mh)}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"room":     g.roomName,
			"speed":    g.baseSpeed,
			"size":     g.fontSize,
			"colors":   g.rawColorsString,
			"outline":  g.rawOutlineString,
			"display":  g.displayMode,
			"windowX":  g.windowX,
			"windowY":  g.windowY,
			"monitors": infos,
			"archive":  g.recentArchive,
		})
	})

	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		var req SettingsRequest
		json.NewDecoder(r.Body).Decode(&req)
		g.mu.Lock()

		g.baseSpeed = req.Speed
		g.fontColors = parseHexColors(req.Colors)
		g.rawColorsString = req.Colors
		g.rawOutlineString = req.Outline
		g.outlineColor = parseOutline(req.Outline)

		if req.Size != g.fontSize {
			f, _ := opentype.NewFace(g.fontTemplate, &opentype.FaceOptions{Size: float64(req.Size), DPI: 72})
			g.mplusNormalFont = f
			g.fontSize = req.Size
		}

		if req.Display != g.displayMode || req.WindowX != g.windowX || req.WindowY != g.windowY {
			g.displayMode = req.Display
			g.windowX = req.WindowX
			g.windowY = req.WindowY
			g.updateDisplay = true
		}

		if g.roomName != req.Room {
			g.roomName = req.Room
			if g.wsConn != nil {
				g.wsConn.Close()
			}
		}
		g.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action string `json:"action"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		g.mu.Lock()
		if req.Action == "disconnect" {
			g.roomName = ""
			if g.wsConn != nil {
				g.wsConn.Close()
			}
		}
		if req.Action == "quit" {
			g.shouldQuit = true
		}
		g.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/export", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Write([]byte{0xEF, 0xBB, 0xBF})
		fmt.Fprintln(w, "Time,Comment")

		// ファイルから全てのエントリを読み込んで出力
		g.mu.Lock()
		archiveFilePath := g.archiveFilePath
		g.mu.Unlock()

		if archiveFilePath != "" {
			data, err := os.ReadFile(archiveFilePath)
			if err == nil {
				lines := strings.Split(string(data), "\n")
				for _, line := range lines {
					if strings.TrimSpace(line) == "" {
						continue
					}
					var entry LogEntry
					if err := json.Unmarshal([]byte(line), &entry); err == nil {
						fmt.Fprintf(w, "%s,\"%s\"\n", entry.Time, strings.ReplaceAll(entry.Comment, "\"", "\"\""))
					}
				}
			}
		}
	})

	var listener net.Listener
	var err error
	port := startPort
	for i := 0; i < 100; i++ {
		listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			break
		}
		port++
	}
	go http.Serve(listener, mux)
	return port
}

func parseOutline(s string) color.Color {
	switch strings.ToLower(s) {
	case "black":
		return color.Black
	case "white":
		return color.White
	case "none":
		return nil
	default:
		c, err := parseHexColor(s)
		if err == nil {
			return c
		}
		return nil
	}
}

func parseHexColor(s string) (c color.RGBA, err error) {
	c.A = 0xff
	if s[0] == '#' {
		s = s[1:]
	}
	_, err = fmt.Sscanf(s, "%02x%02x%02x", &c.R, &c.G, &c.B)
	return
}

func parseHexColors(s string) []color.Color {
	parts := strings.Split(s, ",")
	var colors []color.Color
	for _, p := range parts {
		if c, err := parseHexColor(strings.TrimSpace(p)); err == nil {
			colors = append(colors, c)
		}
	}
	if len(colors) == 0 {
		colors = append(colors, color.White)
	}
	return colors
}

func openAppWindow(url string) {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		// Chrome
		chromePath := `C:\Program Files\Google\Chrome\Application\chrome.exe`
		if _, err := os.Stat(chromePath); err == nil {
			exec.Command(chromePath, "--app="+url).Start()
			return
		}

		// Edge
		edgePath := `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`
		if _, err := os.Stat(edgePath); err == nil {
			exec.Command(edgePath, "--app="+url).Start()
			return
		}

		// 既定のブラウザ
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		cmd = exec.Command("xdg-open", url)
	}

	err := cmd.Start()
	if err != nil {
		log.Printf("Failed to open browser: %v\n", err)
	}
}

func main() {
	roomPtr := flag.String("room", "", "Room name")
	sizePtr := flag.Int("size", 48, "Font size")
	speedPtr := flag.Float64("speed", 4.0, "Base speed")
	colorsPtr := flag.String("colors", "#FFFFFF,#FF0000,#FFFF00,#00FF00,#00FFFF,#FF00FF", "Colors")
	outlinePtr := flag.String("outline", "black", "Outline color")
	displayPtr := flag.String("display", "all", "Display to show comments")
	webPortPtr := flag.Int("port", 8080, "Web UI Port")

	flag.Parse()
	rand.Seed(time.Now().UnixNano())

	fontBytes, err := embedFS.ReadFile("MPLUS1p-Bold.ttf")
	if err != nil {
		log.Fatalf("Error: %v", err)
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

	// アーカイブファイルをセットアップ
	tempDir := filepath.Join(os.TempDir(), "golide_archive")
	os.MkdirAll(tempDir, 0755)
	archiveFilePath := filepath.Join(tempDir, "archive.jsonl")
	archiveFile, err := os.OpenFile(archiveFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("Failed to open archive file: %v", err)
	}

	game := &Game{
		mplusNormalFont:  mplusNormalFont,
		fontTemplate:     tt,
		fontSize:         *sizePtr,
		fontColors:       parseHexColors(*colorsPtr),
		outlineColor:     parseOutline(*outlinePtr),
		roomName:         *roomPtr,
		baseSpeed:        *speedPtr,
		rawColorsString:  *colorsPtr,
		rawOutlineString: *outlinePtr,
		displayMode:      *displayPtr,
		windowX:          0, // 初期化
		windowY:          0, // 初期化
		recentArchive:    make([]LogEntry, 0),
		archiveFile:      archiveFile,
		archiveFilePath:  archiveFilePath,
		shouldQuit:       false,
	}

	// ★起動時に全モニターの幅を合算してサイズを確定
	game.initWindowSize()

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

	// ★ OSには「算出したピッタリのサイズ」を1度だけ伝える
	ebiten.SetWindowSize(game.totalW, game.maxH-1)

	// 初回のクリッピング領域計算
	game.applyLogicalClip()
	// Ebitengine原点誤認への対処（初期は0,0）
	ebiten.SetWindowPosition(game.windowX, game.windowY)

	options := &ebiten.RunGameOptions{
		ScreenTransparent: true,
	}

	if err := ebiten.RunGameWithOptions(game, options); err != nil {
		log.Fatal(err)
	}

	// クリーンアップ
	if game.archiveFile != nil {
		game.archiveFile.Close()
	}
}
