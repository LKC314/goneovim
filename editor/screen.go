package editor

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akiyosi/gonvim/util"
	"github.com/bluele/gcache"
	"github.com/neovim/go-client/nvim"
	"github.com/therecipe/qt/core"
	"github.com/therecipe/qt/gui"
	"github.com/therecipe/qt/widgets"
)

const (
	CACHESIZE int = 256
)

type gridId = int

// Highlight is
type Highlight struct {
	id int
	// kind       string
	uiName        string
	hlName        string
	foreground    *RGBA
	background    *RGBA
	special       *RGBA
	reverse       bool
	italic        bool
	bold          bool
	underline     bool
	undercurl     bool
	strikethrough bool
}

type HlChars struct {
	text string
	fg   *RGBA
	// bg     *RGBA
	italic bool
	bold   bool
}

// type HlChar struct {
// 	char   string
// 	fg     *RGBA
// 	bg     *RGBA
// 	italic bool
// 	bold   bool
// }

// Cell is
type Cell struct {
	normalWidth bool
	char        string
	highlight   Highlight
}

// Window is
type Window struct {
	paintMutex  sync.Mutex
	redrawMutex sync.Mutex

	s       *Screen
	content [][]*Cell
	lenLine []int

	grid    gridId
	id      nvim.Window
	bufName string
	pos     [2]int
	anchor  string
	cols    int
	rows    int

	isMsgGrid  bool
	isFloatWin bool

	widget           *widgets.QWidget
	shown            bool
	queueRedrawArea  [4]int
	scrollRegion     []int
	devicePixelRatio float64
	textCache        gcache.Cache
	// glyphMap         map[HlChar]gui.QImage

	font         *Font
	width        float64
	height       int
	localWindows *[4]localWindow
}

type localWindow struct {
	grid        gridId
	isResized   bool
	localWidth  float64
	localHeight int
}

// Screen is the main editor area
type Screen struct {
	ws   *Workspace
	font *Font

	name    string
	widget  *widgets.QWidget
	windows map[gridId]*Window
	width   int
	height  int

	cursor           [2]int
	scrollRegion     []int
	scrollDust       [2]int
	scrollDustDeltaY int

	highAttrDef    map[int]*Highlight
	highlightGroup map[string]int

	// tooltip *widgets.QLabel
	tooltip *widgets.QLineEdit

	queueRedrawArea [4]int
	textCache        gcache.Cache

	resizeCount     uint
}

func newScreen() *Screen {
	widget := widgets.NewQWidget(nil, 0)
	widget.SetContentsMargins(0, 0, 0, 0)
	widget.SetStyleSheet(" * { background-color: rgba(0, 0, 0, 0);}")

	purgeQimage := func(key, value interface{}) {
		image := value.(*gui.QImage)
		image.DestroyQImage()
	}
	cache := gcache.New(CACHESIZE).LRU().
		EvictedFunc(purgeQimage).
		PurgeVisitorFunc(purgeQimage).
		Build()

	screen := &Screen{
		widget:         widget,
		windows:        make(map[gridId]*Window),
		cursor:         [2]int{0, 0},
		scrollRegion:   []int{0, 0, 0, 0},
		highlightGroup: make(map[string]int),
		textCache:      cache,
	}

	widget.SetAcceptDrops(true)
	widget.ConnectWheelEvent(screen.wheelEvent)
	widget.ConnectDragEnterEvent(screen.dragEnterEvent)
	widget.ConnectDragMoveEvent(screen.dragMoveEvent)
	widget.ConnectDropEvent(screen.dropEvent)
	widget.ConnectMousePressEvent(screen.mouseEvent)
	widget.ConnectMouseReleaseEvent(screen.mouseEvent)
	widget.ConnectMouseMoveEvent(screen.mouseEvent)
	widget.ConnectResizeEvent(func(event *gui.QResizeEvent) {
		screen.updateSize()
	})

	return screen
}

func (s *Screen) initInputMethodWidget() {
	// tooltip := widgets.NewQLabel(s.widget, 0)
	tooltip := widgets.NewQLineEdit(nil)
	tooltip.SetAttribute(core.Qt__WA_InputMethodEnabled, true)
	tooltip.SetVisible(false)
	tooltip.ConnectInputMethodEvent(s.inputmethodevent)
	tooltip.ConnectInputMethodQuery(s.inputmethodquery)
	tooltip.ConnectReturnPressed(func() {
		s.ws.nvim.Input(s.tooltip.Text())
		s.tooltip.SetFocusPolicy(core.Qt__NoFocus);
		s.tooltip.Clear()
		s.tooltip.Hide()
	})
	s.tooltip = tooltip
}

func (s *Screen) inputmethodevent(event *gui.QInputMethodEvent) {
	if event.CommitString() != "" && event.PreeditString() == "" {
		s.tooltip.InputMethodEventDefault(event)
		s.tooltip.ReturnPressed()
	} else {
		s.tooltip.InputMethodEventDefault(event)
	}
}


func (s *Screen) inputmethodquery(query core.Qt__InputMethodQuery) *core.QVariant {
	w := s.ws
	if query == core.Qt__ImCursorRectangle {
		x, y, candX, candY := w.screen.toolTipPos()
		w.screen.toolTipMove(x, y)
		imrect := core.NewQRect()
		imrect.SetRect(candX, candY, 1, w.font.lineHeight)

		if w.palette.widget.IsVisible() {
			w.cursor.x = x
			w.cursor.y = w.palette.patternPadding + w.cursor.shift
			w.cursor.widget.Move2(w.cursor.x, w.cursor.y)
		}

		return core.NewQVariant31(imrect)
	}
	return s.tooltip.InputMethodQueryDefault(query)
}

func (s *Screen) dragEnterEvent(e *gui.QDragEnterEvent) {
	e.AcceptProposedAction()
}

func (s *Screen) dragMoveEvent(e *gui.QDragMoveEvent) {
	e.AcceptProposedAction()
}

func (s *Screen) dropEvent(e *gui.QDropEvent) {
	e.SetDropAction(core.Qt__CopyAction)
	e.AcceptProposedAction()
	e.SetAccepted(true)

	for _, i := range strings.Split(e.MimeData().Text(), "\n") {
		data := strings.Split(i, "://")
		if i != "" {
			switch data[0] {
			case "file":
				buf, _ := s.ws.nvim.CurrentBuffer()
				bufName, _ := s.ws.nvim.BufferName(buf)
				var filepath string
				switch data[1][0] {
				case '/':
					if runtime.GOOS == "windows" {
						filepath = strings.Trim(data[1], `/`)
					} else {
						filepath = data[1]
					}
				default:
					if runtime.GOOS == "windows" {
						filepath = fmt.Sprintf(`//%s`, data[1])
					} else {
						filepath = data[1]
					}
				}

				if bufName != "" {
					s.howToOpen(filepath)
				} else {
					fileOpenInBuf(filepath)
				}
			default:
			}
		}
	}
}

func fileOpenInBuf(file string) {
	isModified, _ := editor.workspaces[editor.active].nvim.CommandOutput("echo &modified")
	if isModified == "1" {
		editor.workspaces[editor.active].nvim.Command(fmt.Sprintf(":tabnew %s", file))
	} else {
		editor.workspaces[editor.active].nvim.Command(fmt.Sprintf(":e %s", file))
	}
}

func (s *Screen) howToOpen(file string) {
	message := fmt.Sprintf("[Gonvvim] Do you want to diff between the file being dropped and the current buffer?")
	opts := []*NotifyButton{}
	opt1 := &NotifyButton{
		action: func() {
			editor.workspaces[editor.active].nvim.Command(fmt.Sprintf(":vertical diffsplit %s", file))
		},
		text: "Yes",
	}
	opts = append(opts, opt1)

	opt2 := &NotifyButton{
		action: func() {
			fileOpenInBuf(file)
		},
		text: "No, I want to open with a new buffer",
	}
	opts = append(opts, opt2)

	editor.pushNotification(NotifyInfo, 0, message, notifyOptionArg(opts))
}

func (s *Screen) updateRows() bool {
	var ret bool
	ws := s.ws
	rows := s.height / s.font.lineHeight

	if rows != ws.rows {
		ret = true
	}
	ws.rows = rows
	return ret
}

func (s *Screen) updateCols() bool {
	var ret bool
	ws := s.ws
	s.width = s.widget.Width()
	cols := int(float64(s.width) / s.font.truewidth)

	if cols != ws.cols {
		ret = true
	}
	ws.cols = cols
	return ret
}

func (s *Screen) waitTime() time.Duration {
	var ret time.Duration
	switch s.resizeCount {
	case 0:
		ret = 10
	case 1:
		ret = 100
	default:
		ret = 1000
	}

	s.resizeCount++
	return ret
}

func (s *Screen) updateSize() {
	ws := s.ws
	s.width = s.widget.Width()
	currentCols := int(float64(s.width) / s.font.truewidth)
	currentRows := s.height / s.font.lineHeight

	isNeedTryResize := (currentCols != ws.cols || currentRows != ws.rows)
	if !isNeedTryResize {
		return
	}

	ws.cols = currentCols
	ws.rows = currentRows

	if !ws.uiAttached {
		return
	}

	for _, win := range s.windows {
		if win == nil {
			continue
		}
		if win.font != nil {
			win.width = 0
			win.height = 0
		}
	}

	s.uiTryResize(currentCols, currentRows)
}

func (s *Screen) uiTryResize(width, height int) {
	ws := s.ws
	done := make(chan error, 5)
	var result error
	go func() {
		result = ws.nvim.TryResizeUI(width, height)
		done <- result
	}()
	select {
	case <-done:
	case <-time.After(s.waitTime() * time.Millisecond):
		// In this case, assuming that nvim is returning an error
		//  at startup and the TryResizeUI() function hangs up.
		ws.nvim.Input("<Enter>")
		s.uiTryResize(width, height)
	}
}

func (s *Screen) gridFont(update interface{}) {
	win := s.windows[s.ws.cursor.gridid]
	if win == nil {
		return
	}

	args := update.(string)
	if args == "" {
		return
	}
	parts := strings.Split(args, ":")
	if len(parts) < 1 {
		return
	}

	fontfamily := parts[0]
	height := 14

	for _, p := range parts[1:] {
		if strings.HasPrefix(p, "h") {
			var err error
			height, err = strconv.Atoi(p[1:])
			if err != nil {
				return
			}
		}
	}

	oldWidth := float64(win.cols) * win.getFont().truewidth
	oldHeight := win.rows * win.getFont().lineHeight
	win.width = oldWidth
	win.height = oldHeight
	win.localWindows = &[4]localWindow{}

	// fontMetrics := gui.NewQFontMetricsF(gui.NewQFont2(fontfamily, height, 1, false))
	win.font = initFontNew(fontfamily, height, 1, false)

	// Calculate new cols, rows of current grid
	newCols := int(float64(oldWidth) / win.font.truewidth)
	newRows := oldHeight / win.font.lineHeight

	// Cache
	if win.textCache == nil {
		purgeQimage := func(key, value interface{}) {
			image := value.(*gui.QImage)
			image.DestroyQImage()
		}
		cache := gcache.New(CACHESIZE).LRU().
			EvictedFunc(purgeQimage).
			PurgeVisitorFunc(purgeQimage).
			Build()
		win.textCache = cache
	} else {
		win.textCache.Purge()
	}

	_ = s.ws.nvim.TryResizeUIGrid(s.ws.cursor.gridid, newCols, newRows)
	font := win.getFont()
	s.ws.cursor.updateFont(font)
}

func (s *Screen) toolTipPos() (int, int, int, int) {
	var x, y, candX, candY int
	ws := s.ws
	if len(s.windows) == 0 {
		return 0, 0, 0, 0
	}
	if ws.palette.widget.IsVisible() {
		s.tooltip.SetParent(s.ws.palette.widget)
		font := gui.NewQFont2(editor.extFontFamily, editor.extFontSize, 1, false)
		s.tooltip.SetFont(font)
		x = ws.palette.cursorX + ws.palette.patternPadding
		candX = x + ws.palette.widget.Pos().X()
		y = ws.palette.patternPadding + ws.palette.padding
		candY = y + ws.palette.widget.Pos().Y()
	} else {
		s.toolTipFont(s.font)
		row := s.cursor[0]
		col := s.cursor[1]
		x = int(float64(col) * s.font.truewidth)
		y = row * s.font.lineHeight
		candX = int(float64(col+s.windows[s.ws.cursor.gridid].pos[0]) * s.font.truewidth)
		candY = (row+s.windows[s.ws.cursor.gridid].pos[1])*s.font.lineHeight + ws.tabline.height + ws.tabline.marginTop + ws.tabline.marginBottom
	}
	return x, y, candX, candY
}

func (s *Screen) toolTipMove(x int, y int) {
	padding := 0
	if s.ws.palette.widget.IsVisible() {
		padding = s.ws.palette.padding
	}
	s.tooltip.Move(core.NewQPoint2(x+padding, y))
}

func (s *Screen) toolTipFont(font *Font) {
	s.tooltip.SetFont(font.fontNew)
	s.tooltip.SetContentsMargins(0, font.lineSpace/2, 0, font.lineSpace/2)
}

func (s *Screen) toolTipShow() {
	if !s.ws.palette.widget.IsVisible() {
		win, ok := s.windows[s.ws.cursor.gridid]
		if ok {
			s.tooltip.SetParent(win.widget)
		}
	}
	s.tooltip.AdjustSize()
	s.tooltip.Show()
}

func (s *Screen) toolTip(text string) {
	s.tooltip.SetText(text)
	s.tooltip.AdjustSize()
	s.toolTipShow()

	row := s.cursor[0]
	col := s.cursor[1]
	c := s.ws.cursor
	c.x = int(float64(col)*s.font.truewidth) + s.tooltip.Width()
	c.y = row * s.font.lineHeight
	c.move()
}

func (w *Window) paint(event *gui.QPaintEvent) {
	w.paintMutex.Lock()

	rect := event.Rect()

	font := w.getFont()
	row := int(float64(rect.Top()) / float64(font.lineHeight))
	col := int(float64(rect.Left()) / font.truewidth)
	rows := int(math.Ceil(float64(rect.Height()) / float64(font.lineHeight)))
	cols := int(math.Ceil(float64(rect.Width()) / font.truewidth))

	p := gui.NewQPainter2(w.widget)
	if w.s.name == "minimap" || !editor.config.Editor.CachedDrawing {
		p.SetFont(font.fontNew)
	}

	// Draw contents
	for y := row; y < row+rows; y++ {
		if y >= w.rows {
			continue
		}
		w.fillBackground(p, y, col, cols)
		w.drawContents(p, y, col, cols)
		w.drawTextDecoration(p, y, col, cols)
	}

	// If Window is Message Area, draw separator
	if w.isMsgGrid {
		w.drawMsgSeparator(p)
	}

	// Draw vim window separator
	w.drawBorders(p, row, col, rows, cols)

	// Draw indent guide
	if editor.config.Editor.IndentGuide {
		w.drawIndentguide(p, row, rows)
		// for _, win := range w.s.windows {
		// 	win.drawIndentguide(p)
		// }
	}

	// Update markdown preview
	if w.grid != 1 {
		w.s.ws.markdown.updatePos()
	}

	p.DestroyQPainter()
	w.paintMutex.Unlock()
}

func (w *Window) getFont() *Font {
	if w.font == nil {
		return w.s.font
	} else {
		return w.font
	}
}

func (w *Window) drawIndentguide(p *gui.QPainter, row, rows int) {
	if w == nil {
		return
	}
	if w.isMsgGrid {
		return
	}
	if w.isFloatWin {
		return
	}
	if w.s.ws.ts == 0 {
		return
	}
	if !w.isShown() {
		return
	}
	for y := row; y < rows; y++ {
		if y+1 >= len(w.content) {
			return
		}
		nextline := w.content[y+1]
		line := w.content[y]
		res := 0
		skipDraw := false
		for x := 0; x < w.lenLine[y]; x++ {
			skipDraw = false

			if x+1 >= len(nextline) {
				break
			}
			nlnc := nextline[x+1]
			if nlnc == nil {
				continue
			}
			nlc := nextline[x]
			if nlc == nil {
				continue
			}
			nc := line[x+1]
			if nc == nil {
				continue
			}
			c := line[x]
			if c == nil {
				continue
			}
			if c.isSignColumn() {
				res++
			}
			if c.char != " " && !c.isSignColumn() {
				break
			}
			if x > res &&
				(x+1-res)%w.s.ws.ts == 0 &&
				c.char == " " && nc.char != " " &&
				nlc.char == " " && nlnc.char == " " {

				if w.lenLine[y] >= len(line) {
					break
				}
				bra := line[w.lenLine[y]-1].char
				cket := getCket(bra)

				for row := y; row < len(w.content); row++ {
					if row+1 == len(w.content) {
						break
					}

					for z := y + 1; z < len(w.content); z++ {
						if w.content[z][x+1] == nil {
							break
						}
						if w.content[z][x+1].char != " " {
							if w.content[z][x+1].char != cket {
								break
							}

							for v := x; v >= res; v-- {
								if w.content[z][v] == nil {
									break
								}
								if w.content[z][v].char == " " {
									skipDraw = true
								} else {
									skipDraw = false
									break
								}
							}
							if skipDraw {
								break
							}
						}
					}
					if !skipDraw {
						break
					}

					ylen, _ := w.countHeadSpaceOfLine(y)
					ynlen, _ := w.countHeadSpaceOfLine(y + 1)
					if ynlen <= ylen {
						break
					}
					if w.content[row+1][x+1] == nil {
						break
					}
					if w.content[row+1][x+1].char != " " {
						break
					}
					w.drawIndentline(p, x+1, row+1)
				}
				break
			}
		}
	}
}

func getCket(bra string) string {
	cket := " "

	switch bra {
	case "{":
		cket = "}"
	case "[":
		cket = "]"
	case "(":
		cket = ")"
	case "<":
		cket = ">"
	case `"`:
		cket = `"`
	case `'`:
		cket = `'`
	case "`":
		cket = "`"
	}

	return cket
}

func (w *Window) drawIndentline(p *gui.QPainter, x int, y int) {
	font := w.getFont()
	X := float64(x) * font.truewidth
	Y := float64(y * font.lineHeight)
	p.FillRect4(
		core.NewQRectF4(
			X,
			Y,
			1,
			float64(font.lineHeight),
		),
		editor.colors.indentGuide.QColor(),
	)
}

func (w *Window) drawMsgSeparator(p *gui.QPainter) {
	color := w.s.highAttrDef[w.s.highlightGroup["MsgSeparator"]].foreground
	p.FillRect4(
		core.NewQRectF4(
			0,
			0,
			float64(w.widget.Width()),
			1,
		),
		gui.NewQColor3(
			color.R,
			color.G,
			color.B,
			200),
	)
}

func (w *Window) drawBorders(p *gui.QPainter, row, col, rows, cols int) {
	if w == nil {
		return
	}
	if w.grid != 1 {
		return
	}
	if !editor.config.Editor.DrawBorder {
		return
	}
	for _, win := range w.s.windows {
		if win == nil {
			continue
		}
		if !win.isShown() {
			continue
		}
		if win.isFloatWin {
			continue
		}
		if win.isMsgGrid {
			continue
		}
		if win.pos[0]+win.cols < row && (win.pos[1]+win.rows+1) < col {
			continue
		}
		if win.pos[0] > (row+rows) && (win.pos[1]+win.rows) > (col+cols) {
			continue
		}
		win.drawBorder(p)
	}

}

func (w *Window) drawBorder(p *gui.QPainter) {
	font := w.getFont()

	// window position is based on cols, rows of global font setting
	x := int(float64(w.pos[0]) * w.s.font.truewidth)
	y := w.pos[1] * int(w.s.font.lineHeight)
	width := int(float64(w.cols) * font.truewidth)
	winHeight := int((float64(w.rows) + 0.92) * float64(font.lineHeight))
	color := editor.colors.windowSeparator.QColor()

	// Vertical
	if y+font.lineHeight+1 < w.s.widget.Height() {

		p.FillRect5(
			int(float64(x+width)+font.truewidth/2),
			y-int(font.lineHeight/2),
			2,
			winHeight,
			color,
		)
	}

	bottomBorderPos := w.pos[1]*w.s.font.lineHeight + w.widget.Rect().Bottom()
	isSkipDrawBottomBorder := bottomBorderPos > w.s.bottomWindowPos()-w.s.font.lineHeight && bottomBorderPos < w.s.bottomWindowPos()+w.s.font.lineHeight
	if isSkipDrawBottomBorder {
		return
	}

	// Horizontal
	height := w.rows * int(font.lineHeight)
	y2 := y + height - 1 + font.lineHeight/2

	p.FillRect5(
		int(float64(x)-font.truewidth/2),
		y2,
		int((float64(w.cols)+0.92)*font.truewidth),
		2,
		color,
	)
}

func (s *Screen) bottomWindowPos() int {
	pos := 0
	for _, win := range s.windows {
		if win == nil {
			continue
		}
		if win.grid == 1 {
			continue
		}
		if win.isMsgGrid {
			continue
		}
		position := win.pos[1]*win.s.font.lineHeight + win.widget.Rect().Bottom()
		if pos < position {
			pos = position
		}
	}

	return pos
}

func (s *Screen) wheelEvent(event *gui.QWheelEvent) {
	var m sync.Mutex
	m.Lock()
	defer m.Unlock()

	var v, h, vert, horiz int
	var vertKey string
	var horizKey string
	var accel int
	font := s.font

	switch runtime.GOOS {
	case "darwin":
		pixels := event.PixelDelta()
		if pixels != nil {
			v = pixels.Y()
			h = pixels.X()
		}
		if pixels.X() < 0 && s.scrollDust[0] > 0 {
			s.scrollDust[0] = 0
		}
		if pixels.Y() < 0 && s.scrollDust[1] > 0 {
			s.scrollDust[1] = 0
		}

		dx := math.Abs(float64(s.scrollDust[0]))
		dy := math.Abs(float64(s.scrollDust[1]))

		fontheight := float64(font.lineHeight)
		fontwidth := float64(font.truewidth)

		s.scrollDust[0] += h
		s.scrollDust[1] += v

		if dx >= fontwidth {
			horiz = int(math.Trunc(float64(s.scrollDust[0]) / fontwidth))
			s.scrollDust[0] = 0
		}
		if dy >= fontheight {
			vert = int(math.Trunc(float64(s.scrollDust[1]) / fontheight))
			s.scrollDust[1] = 0
		}

		s.scrollDustDeltaY = int(math.Abs(float64(vert)) - float64(s.scrollDustDeltaY))
		if s.scrollDustDeltaY < 1 {
			s.scrollDustDeltaY = 0
		}
		if s.scrollDustDeltaY <= 2 {
			accel = 1
		} else if s.scrollDustDeltaY > 2 {
			accel = int(float64(s.scrollDustDeltaY) / float64(4))
		}
		if accel == 0 {
			accel = 1
		}

	default:
		vert = event.AngleDelta().Y()
		horiz = event.AngleDelta().X()
		accel = 2
	}

	mod := event.Modifiers()

	if vert > 0 {
		vertKey = "Up"
	} else {
		vertKey = "Down"
	}
	if horiz > 0 {
		horizKey = "Left"
	} else {
		horizKey = "Right"
	}

	if vert == 0 && horiz == 0 {
		return
	}

	s.focusWindow(event)

	mode := s.ws.mode
	if mode == "insert" {
		s.ws.nvim.Input("<Esc>")
	} else if mode == "terminal-input" {
		s.ws.nvim.Input(`<C-\><C-n>`)
	}

	x := int(float64(event.X()) / font.truewidth)
	y := int(float64(event.Y()) / float64(font.lineHeight))
	pos := []int{x, y}

	// if vert > 0 {
	// 	s.ws.nvim.Input(fmt.Sprintf("%v<C-Y>", accel))
	// } else if vert < 0 {
	// 	s.ws.nvim.Input(fmt.Sprintf("%v<C-E>", accel))
	// }
	if vert != 0 {
		// s.ws.nvim.Input(fmt.Sprintf("%v<%sScrollWheel%s><%d,%d>", accel, editor.modPrefix(mod), vertKey, pos[0], pos[1]))
		s.ws.nvim.Input(fmt.Sprintf("<%sScrollWheel%s>", editor.modPrefix(mod), vertKey))
	}

	if horiz != 0 {
		s.ws.nvim.Input(fmt.Sprintf("<%sScrollWheel%s><%d,%d>", editor.modPrefix(mod), horizKey, pos[0], pos[1]))
	}

	event.Accept()
}

func (s *Screen) focusWindow(event *gui.QWheelEvent) {
	mod := event.Modifiers()
	col := int(float64(event.X()) / s.font.truewidth)
	row := int(float64(event.Y()) / float64(s.font.lineHeight))
	for _, win := range s.windows {
		if win == nil {
			continue
		}
		if win.grid == 1 {
			continue
		}
		if win.isMsgGrid {
			continue
		}
		X := event.X()
		Y := event.Y()
		rect := win.widget.Geometry()
		if rect.Contains3(X, Y) {
			s.ws.nvim.InputMouse("left", "press", editor.modPrefix(mod), win.grid, row, col)
			s.ws.nvim.InputMouse("left", "release", editor.modPrefix(mod), win.grid, row, col)
			go s.ws.nvim.Input("<Esc>")

			return
		}
	}
}

func (s *Screen) mouseEvent(event *gui.QMouseEvent) {
	inp := s.convertMouse(event)
	if inp == "" {
		return
	}
	s.ws.nvim.Input(inp)
}

func (s *Screen) convertMouse(event *gui.QMouseEvent) string {
	font := s.font
	x := int(float64(event.X()) / font.truewidth)
	y := int(float64(event.Y()) / float64(font.lineHeight))
	pos := []int{x, y}

	bt := event.Button()
	if event.Type() == core.QEvent__MouseMove {
		if event.Buttons()&core.Qt__LeftButton > 0 {
			bt = core.Qt__LeftButton
		} else if event.Buttons()&core.Qt__RightButton > 0 {
			bt = core.Qt__RightButton
		} else if event.Buttons()&core.Qt__MidButton > 0 {
			bt = core.Qt__MidButton
		} else {
			return ""
		}
	}

	mod := event.Modifiers()
	buttonName := ""
	switch bt {
	case core.Qt__LeftButton:
		buttonName += "Left"
	case core.Qt__RightButton:
		buttonName += "Right"
	case core.Qt__MidButton:
		buttonName += "Middle"
	case core.Qt__NoButton:
	default:
		return ""
	}

	evType := ""
	switch event.Type() {
	case core.QEvent__MouseButtonDblClick:
		evType += "Mouse"
	case core.QEvent__MouseButtonPress:
		evType += "Mouse"
	case core.QEvent__MouseButtonRelease:
		evType += "Release"
	case core.QEvent__MouseMove:
		evType += "Drag"
	default:
		return ""
	}

	return fmt.Sprintf("<%s%s%s><%d,%d>", editor.modPrefix(mod), buttonName, evType, pos[0], pos[1])
}

func (s *Screen) gridResize(args []interface{}) {
	var gridid gridId
	var rows, cols int
	for _, arg := range args {
		gridid = util.ReflectToInt(arg.([]interface{})[0])
		cols = util.ReflectToInt(arg.([]interface{})[1])
		rows = util.ReflectToInt(arg.([]interface{})[2])

		if isSkipGlobalId(gridid) {
			continue
		}

		s.resizeWindow(gridid, cols, rows)
	}
}

func (s *Screen) resizeWindow(gridid gridId, cols int, rows int) {
	win := s.windows[gridid]
	if win != nil {
		if win.cols == cols && win.rows == rows {
			return
		}
	}

	// make new size content
	content := make([][]*Cell, rows)
	lenLine := make([]int, rows)

	for i := 0; i < rows; i++ {
		content[i] = make([]*Cell, cols)
	}

	if win != nil && gridid != 1 {
		for i := 0; i < rows; i++ {
			if i >= len(win.content) {
				continue
			}
			lenLine[i] = win.lenLine[i]
			for j := 0; j < cols; j++ {
				if j >= len(win.content[i]) {
					continue
				}
				content[i][j] = win.content[i][j]
			}
		}
	}

	if win == nil {
		s.windows[gridid] = newWindow()
		s.windows[gridid].s = s
		s.windows[gridid].setParent(s.widget)
		s.windows[gridid].grid = gridid
		s.windows[gridid].widget.SetAttribute(core.Qt__WA_KeyCompression, true)
		// reassign win
		win = s.windows[gridid]

		// first cursor pos at startup app
		if gridid == 1 && s.name != "minimap" {
			s.ws.cursor.widget.SetParent(win.widget)
		}
	}
	winOldCols := win.cols
	winOldRows := win.rows

	win.lenLine = lenLine
	win.content = content
	win.cols = cols
	win.rows = rows

	s.resizeIndependentFontGrid(win, winOldCols, winOldRows)

	font := win.getFont()
	width := int(float64(cols) * font.truewidth)
	height := rows * int(font.lineHeight)
	rect := core.NewQRect4(0, 0, width, height)
	win.setGeometryAndPalette(rect)

	win.move(win.pos[0], win.pos[1])

	win.show()
	win.queueRedrawAll()
}

func (s *Screen) resizeIndependentFontGrid(win *Window, oldCols, oldRows int) {
	var isExistMsgGrid bool
	for _, w := range s.windows {
		if w == nil {
			continue
		}
		if w.isMsgGrid {
			isExistMsgGrid = true
		}
	}
	if !isExistMsgGrid && len(s.windows) < 3 {
		return
	}
	if isExistMsgGrid && len(s.windows) < 4 {
		return
	}
	if oldCols == 0 && oldRows == 0 {
		return
	}
	// var width, height int
	deltaCols := win.cols - oldCols
	deltaRows := win.rows - oldRows
	absDeltaCols := math.Abs(float64(deltaCols))
	absDeltaRows := math.Abs(float64(deltaRows))
	if absDeltaCols > 0 && absDeltaRows > 0 {
		return
	}
	var isResizeWidth, isResizeHeight bool
	if absDeltaCols > 0 {
		isResizeWidth = true
	} else if absDeltaRows > 0 {
		isResizeHeight = true
	}

	leftWindowPos := win.pos[0] + oldCols + 1 + deltaCols
	topWindowPos := win.pos[1] + oldRows + 1 + deltaRows

	for _, w := range s.windows {
		if w == nil {
			continue
		}
		if w.grid == 1 {
			continue
		}
		if w.isMsgGrid {
			continue
		}
		if w.grid == win.grid {
			continue
		}
		if w.font == nil {
			continue
		}

		if isResizeWidth && w.width > 0 {

			// right window is gridfont window
			if w.localWindows[2].grid == win.grid || (w.pos[1] == win.pos[1] && w.pos[0] == leftWindowPos) {
				if w.localWindows[2].grid == 0 {
					w.localWindows[2].grid = win.grid
				}
				if !w.localWindows[2].isResized {
					w.localWindows[2].isResized = true
					w.localWindows[2].localWidth = w.width + float64(oldCols)*win.getFont().truewidth
				}
				newWidth := w.localWindows[2].localWidth - (float64(win.cols) * win.getFont().truewidth)
				newCols := int(newWidth / w.font.truewidth)
				if newCols != w.cols {
					_ = s.ws.nvim.TryResizeUIGrid(w.grid, newCols, w.rows)
					w.width = float64(newCols) * w.getFont().truewidth
					w.localWindows[0].isResized = false
				}
			}

			// left window is gridfont window
			// calcurate win window posision aa w window coordinate
			var resizeflag bool
			winPosX := float64(win.pos[0]) * win.s.font.truewidth
			rightWindowPos1 := float64(w.cols)*w.getFont().truewidth + float64(w.pos[0]+1-deltaCols+1)*win.s.font.truewidth
			rightWindowPos2 := float64(w.cols-1)*w.getFont().truewidth + float64(w.pos[0]+1-deltaCols+1)*win.s.font.truewidth
			rightWindowPos := int(float64(w.cols)*w.getFont().truewidth/win.s.font.truewidth) + w.pos[0] + 1 - deltaCols + 1
			if win.s.font.truewidth < w.getFont().truewidth {
				resizeflag = winPosX <= rightWindowPos1 && winPosX >= rightWindowPos2
			} else {
				resizeflag = win.pos[0] == rightWindowPos
			}
			if w.localWindows[0].grid == win.grid || (w.pos[1] == win.pos[1] && resizeflag) {
				if w.localWindows[0].grid == 0 {
					w.localWindows[0].grid = win.grid
				}
				if !w.localWindows[0].isResized {
					w.localWindows[0].isResized = true
					w.localWindows[0].localWidth = w.width + float64(oldCols)*win.getFont().truewidth
				}
				newWidth := w.localWindows[0].localWidth - (float64(win.cols) * win.getFont().truewidth)
				newCols := int(newWidth / w.font.truewidth)
				if newCols != w.cols {
					_ = s.ws.nvim.TryResizeUIGrid(w.grid, newCols, w.rows)
					w.width = float64(newCols) * w.getFont().truewidth
					w.localWindows[2].isResized = false
				}
			}

		}
		if isResizeHeight && w.height > 0 {
			// bottom window is gridfont window
			if w.localWindows[1].grid == win.grid || (w.pos[0] == win.pos[0] && w.pos[1] == topWindowPos) {
				if w.localWindows[1].grid == 0 {
					w.localWindows[1].grid = win.grid
				}
				if !w.localWindows[1].isResized {
					w.localWindows[1].isResized = true
					w.localWindows[1].localHeight = w.height + oldRows*win.getFont().lineHeight
				}
				newHeight := w.localWindows[1].localHeight - (win.rows * win.getFont().lineHeight)
				newRows := int(newHeight / w.font.lineHeight)
				if newRows != w.rows {
					_ = s.ws.nvim.TryResizeUIGrid(w.grid, w.cols, newRows)
					w.height = newRows * w.getFont().lineHeight
					w.localWindows[3].isResized = false
				}
			}

			// top window is gridfont window
			// calcurate win window posision aa w window coordinate
			var resizeflag bool
			winPosY := win.pos[1] * win.s.font.lineHeight
			bottomWindowPos1 := w.rows*w.getFont().lineHeight + (w.pos[1]+1-deltaRows+1)*win.s.font.lineHeight
			bottomWindowPos2 := (w.rows-1)*w.getFont().lineHeight + (w.pos[1]+1-deltaRows+1)*win.s.font.lineHeight
			bottomWindowPos := int(w.rows*w.getFont().lineHeight/win.s.font.lineHeight) + w.pos[1] + 1 - deltaRows + 1
			if win.s.font.lineHeight < w.getFont().lineHeight {
				resizeflag = winPosY <= bottomWindowPos1 && winPosY >= bottomWindowPos2
			} else {
				resizeflag = win.pos[1] == bottomWindowPos
			}
			if w.localWindows[3].grid == win.grid || (w.pos[0] == win.pos[0] && resizeflag) {
				if w.localWindows[3].grid == 0 {
					w.localWindows[3].grid = win.grid
				}
				if !w.localWindows[3].isResized {
					w.localWindows[3].isResized = true
					w.localWindows[3].localHeight = w.height + oldRows*win.getFont().lineHeight
				}
				newHeight := w.localWindows[3].localHeight - (win.rows * win.getFont().lineHeight)
				newRows := int(newHeight / w.font.lineHeight)
				if newRows != w.rows {
					_ = s.ws.nvim.TryResizeUIGrid(w.grid, w.cols, newRows)
					w.height = newRows * w.getFont().lineHeight
					w.localWindows[1].isResized = false
				}
			}

		}

	}
}

func (s *Screen) gridCursorGoto(args []interface{}) {
	for _, arg := range args {
		gridid := util.ReflectToInt(arg.([]interface{})[0])
		s.cursor[0] = util.ReflectToInt(arg.([]interface{})[1])
		s.cursor[1] = util.ReflectToInt(arg.([]interface{})[2])
		if isSkipGlobalId(gridid) {
			continue
		}

		if s.windows[gridid] == nil {
			continue
		}

		if s.ws.cursor.gridid != gridid {
			s.ws.cursor.gridid = gridid
			s.windows[gridid].raise()
		}
	}
}

func (s *Screen) setHighAttrDef(args []interface{}) {
	var h map[int]*Highlight
	if s.highAttrDef == nil {
		h = make(map[int]*Highlight)
	} else {
		h = s.highAttrDef
	}
	h[0] = &Highlight{
		foreground: editor.colors.fg,
		background: editor.colors.bg,
	}

	for _, arg := range args {
		id := util.ReflectToInt(arg.([]interface{})[0])
		h[id] = s.getHighlight(arg)
	}

	s.highAttrDef = h
}

func (s *Screen) setHighlightGroup(args []interface{}) {
	for _, arg := range args {
		a := arg.([]interface{})
		hlName := a[0].(string)
		hlIndex := util.ReflectToInt(a[1])
		s.highlightGroup[hlName] = hlIndex
	}
}

func (s *Screen) getHighlight(args interface{}) *Highlight {
	arg := args.([]interface{})
	highlight := Highlight{}

	hl := arg[1].(map[string]interface{})
	info := make(map[string]interface{})
	for _, arg := range arg[3].([]interface{}) {
		info = arg.(map[string]interface{})
		break
	}

	// kind, ok := info["kind"]
	// if ok {
	// 	highlight.kind = kind.(string)
	// }

	id, ok := info["id"]
	if ok {
		highlight.id = util.ReflectToInt(id)
	}

	uiName, ok := info["ui_name"]
	if ok {
		highlight.uiName = uiName.(string)
	}

	hlName, ok := info["hi_name"]
	if ok {
		highlight.hlName = hlName.(string)
	}

	italic := hl["italic"]
	if italic != nil {
		highlight.italic = true
	} else {
		highlight.italic = false
	}

	bold := hl["bold"]
	if bold != nil {
		highlight.bold = true
	} else {
		highlight.bold = false
	}

	underline := hl["underline"]
	if underline != nil {
		highlight.underline = true
	} else {
		highlight.underline = false
	}

	undercurl := hl["undercurl"]
	if undercurl != nil {
		highlight.undercurl = true
	} else {
		highlight.undercurl = false
	}

	strikethrough := hl["strikethrough"]
	if strikethrough != nil {
		highlight.strikethrough = true
	} else {
		highlight.strikethrough = false
	}

	reverse := hl["reverse"]
	if reverse != nil {
		highlight.reverse = true
	} else {
		highlight.reverse = false
	}

	fg, ok := hl["foreground"]
	if ok {
		rgba := calcColor(util.ReflectToInt(fg))
		highlight.foreground = rgba
	}
	if highlight.foreground == nil {
		highlight.foreground = s.ws.foreground
	}

	bg, ok := hl["background"]
	if ok {
		rgba := calcColor(util.ReflectToInt(bg))
		highlight.background = rgba
	}
	if highlight.background == nil {
		highlight.background = s.ws.background
	}

	sp, ok := hl["special"]
	if ok {
		rgba := calcColor(util.ReflectToInt(sp))
		highlight.special = rgba
	} else {
		highlight.special = highlight.foreground
	}

	return &highlight
}

func (h *Highlight) fg() *RGBA {
	var color *RGBA
	if h.reverse {
		color = h.background
		if color == nil {
			// color = w.s.ws.background
			color = editor.colors.bg
		}
	} else {
		color = h.foreground
		if color == nil {
			// color = w.s.ws.foreground
			color = editor.colors.fg
		}
	}

	return color
}

func (h *Highlight) bg() *RGBA {
	var color *RGBA
	if h.reverse {
		color = h.foreground
		if color == nil {
			// color = w.s.ws.foreground
			color = editor.colors.fg
		}
	} else {
		color = h.background
		if color == nil {
			// color = w.s.ws.background
			color = editor.colors.bg
		}
	}

	return color
}

func (s *Screen) gridClear(args []interface{}) {
	var gridid gridId
	for _, arg := range args {
		gridid = util.ReflectToInt(arg.([]interface{})[0])
		if isSkipGlobalId(gridid) {
			continue
		}

		s.windows[gridid].content = make([][]*Cell, s.windows[gridid].rows)
		s.windows[gridid].lenLine = make([]int, s.windows[gridid].rows)

		for i := 0; i < s.windows[gridid].rows; i++ {
			s.windows[gridid].content[i] = make([]*Cell, s.windows[gridid].cols)
		}
		s.windows[gridid].queueRedrawAll()
	}
}

func (s *Screen) gridLine(args []interface{}) {
	for _, arg := range args {
		gridid := util.ReflectToInt(arg.([]interface{})[0])
		if isSkipGlobalId(gridid) {
			continue
		}

		s.updateGridContent(arg.([]interface{}))
		if !s.windows[gridid].isShown() {
			s.windows[gridid].show()
		}
	}
}

func (s *Screen) updateGridContent(arg []interface{}) {
	gridid := util.ReflectToInt(arg[0])
	row := util.ReflectToInt(arg[1])
	colStart := util.ReflectToInt(arg[2])

	if isSkipGlobalId(gridid) {
		return
	}
	if editor.config.Editor.DrawBorder && gridid == 1 {
		return
	}
	if colStart < 0 {
		return
	}

	content := s.windows[gridid].content
	if row >= s.windows[gridid].rows {
		return
	}
	col := colStart
	line := content[row]
	cells := arg[3].([]interface{})

	buffLenLine := colStart
	lenLine := 0

	countLenLine := false
	for _, arg := range cells {
		if col >= len(line) {
			continue
		}
		cell := arg.([]interface{})

		var hi, repeat int
		hi = -1
		text := cell[0]
		if len(cell) >= 2 {
			hi = util.ReflectToInt(cell[1])
		}

		if len(cell) == 3 {
			repeat = util.ReflectToInt(cell[2])
		}

		// If `repeat` is present, the cell should be
		// repeated `repeat` times (including the first time), otherwise just
		// once.
		r := 1
		if repeat == 0 {
			repeat = 1
		}
		for r <= repeat {
			if col >= len(line) {
				break
			}

			if line[col] == nil {
				line[col] = &Cell{}
			}

			line[col].char = text.(string)
			line[col].normalWidth = s.windows[gridid].isNormalWidth(line[col].char)

			// If `hl_id` is not present the most recently seen `hl_id` in
			//	the same call should be used (it is always sent for the first
			//	cell in the event).
			switch col {
			case 0:
				line[col].highlight = *s.highAttrDef[hi]
			default:
				if hi == -1 {
					line[col].highlight = line[col-1].highlight
				} else {
					line[col].highlight = *s.highAttrDef[hi]
				}
			}

			// Count Line content length
			buffLenLine++
			if line[col].char != " " {
				countLenLine = true
			} else if line[col].char == " " {
				countLenLine = false
			}
			if countLenLine {
				lenLine += buffLenLine
				buffLenLine = 0
				countLenLine = false
			}

			col++
			r++
		}

		s.windows[gridid].queueRedraw(0, row, s.windows[gridid].cols, 1)
	}

	// If the array of cell changes doesn't reach to the end of the line,
	// the rest should remain unchanged.
	buffLenLine = 0
	if lenLine < s.windows[gridid].lenLine[row] {
		for x := lenLine; x < s.windows[gridid].lenLine[row]; x++ {
			if x >= len(line) {
				break
			}
			if line[x] == nil {
				break
			}
			// Count Line content length
			buffLenLine++
			if line[x].char != " " {
				countLenLine = true
			} else if line[x].char == " " {
				countLenLine = false
			}
			if countLenLine {
				lenLine += buffLenLine
				buffLenLine = 0
				countLenLine = false
			}
		}
	}
	// Set content length of line
	s.windows[gridid].lenLine[row] = lenLine

	return
}

func (w *Window) countHeadSpaceOfLine(y int) (int, error) {
	if w == nil {
		return 0, errors.New("window is nil")
	}
	if y >= len(w.content) || w.content == nil {
		return 0, errors.New("content is nil")
	}
	line := w.content[y]
	count := 0
	for _, c := range line {
		if c == nil {
			continue
		}

		if c.char != " " && !c.isSignColumn() {
			break
		} else {
			count++
		}
	}
	if count == len(line) {
		count = 0
	}
	return count, nil
}

func (c *Cell) isSignColumn() bool {
	switch c.highlight.hlName {
	case "SignColumn",
		"LineNr",
		"ALEErrorSign",
		"ALEStyleErrorSign",
		"ALEWarningSign",
		"ALEStyleWarningSign",
		"ALEInfoSign",
		"ALESignColumnWithErrors",
		"LspErrorHighlight",
		"LspWarningHighlight",
		"LspInformationHighlight",
		"LspHintHighlight":
		return true
	default:
		return false

	}
}

// func (c *Cell) isStatuslineOrVertSplit() bool {
// 	// If ext_statusline is implemented in Neovim, the implementation may be revised
// 	if &c.highlight == nil {
// 		return false
// 	}
// 	if c.highlight.hlName == "StatusLine" || c.highlight.hlName == "StatusLineNC" || c.highlight.hlName == "VertSplit" {
// 		if editor.config.Editor.DrawBorder {
// 			return true
// 		}
// 	}
// 	return false
// }

func (s *Screen) gridScroll(args []interface{}) {
	var gridid gridId
	var rows int
	for _, arg := range args {
		gridid = util.ReflectToInt(arg.([]interface{})[0])
		if isSkipGlobalId(gridid) {
			continue
		}
		s.windows[gridid].scrollRegion[0] = util.ReflectToInt(arg.([]interface{})[1])     // top
		s.windows[gridid].scrollRegion[1] = util.ReflectToInt(arg.([]interface{})[2]) - 1 // bot
		s.windows[gridid].scrollRegion[2] = util.ReflectToInt(arg.([]interface{})[3])     // left
		s.windows[gridid].scrollRegion[3] = util.ReflectToInt(arg.([]interface{})[4]) - 1 // right
		rows = util.ReflectToInt(arg.([]interface{})[5])
		s.windows[gridid].scroll(rows)
	}
}

func (w *Window) scroll(count int) {
	top := w.scrollRegion[0]
	bot := w.scrollRegion[1]
	left := w.scrollRegion[2]
	right := w.scrollRegion[3]
	content := w.content
	lenLine := w.lenLine

	if top == 0 && bot == 0 && left == 0 && right == 0 {
		top = 0
		bot = w.rows - 1
		left = 0
		right = w.cols - 1
	}

	if count > 0 {
		for row := top; row <= bot-count; row++ {
			if len(content) <= row+count {
				continue
			}

			// copy(content[row], content[row+count])
			for col := left; col <= right; col++ {
				content[row][col] = content[row+count][col]
			}
			lenLine[row] = lenLine[row+count]
		}
		for row := bot - count + 1; row <= bot; row++ {
			for col := left; col <= right; col++ {
				content[row][col] = nil
			}
		}
	} else {
		for row := bot; row >= top-count; row-- {
			if len(content) <= row {
				continue
			}

			// copy(content[row], content[row+count])
			for col := left; col <= right; col++ {
				content[row][col] = content[row+count][col]
			}
			lenLine[row] = lenLine[row+count]
		}
		for row := top; row < top-count; row++ {
			for col := left; col <= right; col++ {
				content[row][col] = nil
			}
		}
	}

	w.queueRedraw(left, top, (right - left + 1), (bot - top + 1))
}

func (w *Window) update() {
	if w == nil {
		return
	}
	if w.queueRedrawArea[2] == 0 && w.queueRedrawArea[3] == 0 {
		return
	}

	font := w.getFont()
	x := int(float64(w.queueRedrawArea[0]) * font.truewidth)
	y := w.queueRedrawArea[1] * font.lineHeight
	width := int(float64(w.queueRedrawArea[2]-w.queueRedrawArea[0]) * font.truewidth)
	height := (w.queueRedrawArea[3] - w.queueRedrawArea[1]) * font.lineHeight

	if width > 0 && height > 0 {
		w.widget.Update2(
			x,
			y,
			width,
			height,
		)
	}

	w.queueRedrawArea[0] = w.cols
	w.queueRedrawArea[1] = w.rows
	w.queueRedrawArea[2] = 0
	w.queueRedrawArea[3] = 0
}

func (s *Screen) update() {
	for _, win := range s.windows {
		if win != nil {
			win.update()
		}
	}
}

func (w *Window) queueRedrawAll() {
	w.queueRedrawArea = [4]int{0, 0, w.cols, w.rows}
}

func (w *Window) queueRedraw(x, y, width, height int) {
	if x < w.queueRedrawArea[0] {
		w.queueRedrawArea[0] = x
	}
	if y < w.queueRedrawArea[1] {
		w.queueRedrawArea[1] = y
	}
	if (x + width) > w.queueRedrawArea[2] {
		w.queueRedrawArea[2] = x + width
	}
	if (y + height) > w.queueRedrawArea[3] {
		w.queueRedrawArea[3] = y + height
	}
}

func (w *Window) transparent(bg *RGBA) int {
	t := 255
	transparent := int(math.Trunc(editor.config.Editor.Transparent * float64(255)))

	if w.s.ws.background.equals(bg) {
		t = 0
	} else {
		t = transparent
	}
	return t
}

func (w *Window) fillHightlight(p *gui.QPainter, y int, col int, cols int) {
	if y >= len(w.content) {
		return
	}
	font := w.getFont()
	line := w.content[y]
	start := -1
	end := -1
	var lastBg *RGBA
	var bg *RGBA
	var lastCell *Cell
	for x := col; x < col+cols; x++ {
		if x >= len(line) {
			continue
		}
		if line[x] == nil {
			continue
		}
		if line[x].char != " " {
			continue
		}
		if line[x] != nil {
			bg = line[x].highlight.background
		} else {
			bg = nil
		}
		if lastCell != nil && !lastCell.normalWidth {
			bg = lastCell.highlight.background
		}
		if bg != nil {
			if lastBg == nil {
				start = x
				end = x
				lastBg = bg
			} else {
				if lastBg.equals(bg) {
					end = x
				} else {
					// Draw a background only when it's color differs from the entire background
					if !lastBg.equals(editor.colors.bg) {
						// last bg is different; draw the previous and start a new one
						rectF := core.NewQRectF4(
							float64(start)*font.truewidth,
							float64((y)*font.lineHeight),
							float64(end-start+1)*font.truewidth,
							float64(font.lineHeight),
						)
						p.FillRect(
							rectF,
							gui.NewQBrush3(
								gui.NewQColor3(
									lastBg.R,
									lastBg.G,
									lastBg.B,
									w.transparent(lastBg),
								),
								core.Qt__SolidPattern,
							),
						)
					}

					// start a new one
					start = x
					end = x
					lastBg = bg
				}
			}
		} else {
			if lastBg != nil {
				if !lastBg.equals(editor.colors.bg) {
					rectF := core.NewQRectF4(
						float64(start)*font.truewidth,
						float64((y)*font.lineHeight),
						float64(end-start+1)*font.truewidth,
						float64(font.lineHeight),
					)
					p.FillRect(
						rectF,
						gui.NewQBrush3(
							gui.NewQColor3(
								lastBg.R,
								lastBg.G,
								lastBg.B,
								w.transparent(lastBg),
							),
							core.Qt__SolidPattern,
						),
					)
				}

				// start a new one
				start = x
				end = x
				lastBg = nil
			}
		}
		lastCell = line[x]
	}
	if lastBg != nil {
		if !lastBg.equals(editor.colors.bg) {
			rectF := core.NewQRectF4(
				float64(start)*font.truewidth,
				float64((y)*font.lineHeight),
				float64(end-start+1)*font.truewidth,
				float64(font.lineHeight),
			)
			p.FillRect(
				rectF,
				gui.NewQBrush3(
					gui.NewQColor3(
						lastBg.R,
						lastBg.G,
						lastBg.B,
						w.transparent(lastBg),
					),
					core.Qt__SolidPattern,
				),
			)
		}
	}
}

func (w *Window) fillBackground(p *gui.QPainter, y int, col int, cols int) {
	if y >= len(w.content) {
		return
	}
	font := w.getFont()
	line := w.content[y]
	var bg *RGBA

	// draw default background color if window is float window or msg grid
	idDrawDefaultBg := false
	if w.isFloatWin || w.isMsgGrid {
		idDrawDefaultBg = true
	}

	var start, end, width int
	var lastBg *RGBA
	var lastHighlight, highlight *Highlight

	for x := col; x < col+cols; x++ {

		fillCellRect := func() {
			if lastHighlight == nil {
				return
			}
			width = end - start + 1
			if width < 0 {
				width = 0
			}
			if !idDrawDefaultBg && lastBg.equals(w.s.ws.background) {
				width = 0
			}
			if width > 0 {
				// Set diff pattern
				pattern, color, transparent := getFillpatternAndTransparent(lastHighlight)

				// Fill background with pattern
				rectF := core.NewQRectF4(
					float64(start)*font.truewidth,
					float64((y)*font.lineHeight),
					float64(width)*font.truewidth,
					float64(font.lineHeight),
				)
				p.FillRect(
					rectF,
					gui.NewQBrush3(
						gui.NewQColor3(
							color.R,
							color.G,
							color.B,
							transparent,
						),
						pattern,
					),
				)
				width = 0
			}
		}

		if x >= len(line) {
			continue
		}

		if line[x] == nil {
			highlight = w.s.highAttrDef[0]
		} else {
			highlight = &line[x].highlight
		}

		bg = highlight.bg()

		if lastBg == nil {
			start = x
			end = x
			lastBg = bg
			lastHighlight = highlight
		}
		if lastBg != nil {
			if lastBg.equals(bg) {
				end = x
			}
			if !lastBg.equals(bg) || x+1 == col+cols {
				fillCellRect()

				start = x
				end = x
				lastBg = bg
				lastHighlight = highlight

				if x+1 == col+cols {
					fillCellRect()
				}
			}
		}
	}
}

func (w *Window) drawTextWithCache(p *gui.QPainter, y int, col int, cols int) {
	if y >= len(w.content) {
		return
	}
	wsfont := w.getFont()
	// font := p.Font()
	line := w.content[y]
	chars := map[Highlight][]int{}
	specialChars := []int{}
	textCache := w.getCache()
	var image *gui.QImage

	for x := col; x < col+cols; x++ {
		if x > w.lenLine[y] {
			continue
		}
		if x >= len(line) {
			continue
		}
		if line[x] == nil {
			continue
		}
		if line[x].char == "" {
			continue
		}
		if line[x].char == " " {
			continue
		}
		if !line[x].normalWidth {
			specialChars = append(specialChars, x)
			continue
		}

		highlight := line[x].highlight
		colorSlice, ok := chars[highlight]
		if !ok {
			colorSlice = []int{}
		}
		colorSlice = append(colorSlice, x)
		chars[highlight] = colorSlice

	}

	pointF := core.NewQPointF3(
		float64(col)*wsfont.truewidth,
		float64(y*wsfont.lineHeight),
	)

	for highlight, colorSlice := range chars {
		var buffer bytes.Buffer
		slice := colorSlice[:]
		for x := col; x < col+cols; x++ {
			if len(slice) == 0 {
				break
			}
			index := slice[0]
			if x < index {
				buffer.WriteString(" ")
				continue
			}
			if x == index {
				buffer.WriteString(line[x].char)
				slice = slice[1:]
			}
		}

		text := buffer.String()
		if text != "" {
			imagev, err := textCache.Get(HlChars{
				text:   text,
				fg:     highlight.fg(),
				italic: highlight.italic,
				bold:   highlight.bold,
			})
			if err != nil {
				image = w.newTextCache(p, text, highlight, true)
			} else {
				image = imagev.(*gui.QImage)
			}
			p.DrawImage7(
				pointF,
				image,
			)
		}

	}

	for _, x := range specialChars {
		if line[x] == nil || line[x].char == " " {
			continue
		}
		imagev, err := textCache.Get(HlChars{
			text:   line[x].char,
			fg:     line[x].highlight.fg(),
			italic: line[x].highlight.italic,
			bold:   line[x].highlight.bold,
		})
		if err != nil {
			image = w.newTextCache(p, line[x].char, line[x].highlight, false)
		} else {
			image = imagev.(*gui.QImage)
		}
		p.DrawImage7(
			core.NewQPointF3(
				float64(x)*wsfont.truewidth,
				float64(y*wsfont.lineHeight),
			),
			image,
		)
	}
}

func (w *Window) newTextCache(p *gui.QPainter, text string, highlight Highlight, isNormalWidth bool) *gui.QImage {
	// * Ref: https://stackoverflow.com/questions/40458515/a-best-way-to-draw-a-lot-of-independent-characters-in-qt5/40476430#40476430

	font := w.getFont()
	width := float64(len(text)) * font.italicWidth
	fg := highlight.fg()
	if !isNormalWidth {
		width = math.Ceil(font.fontMetrics.HorizontalAdvance(text, -1))
	}

	// QImage default device pixel ratio is 1.0,
	// So we set the correct device pixel ratio
	image := gui.NewQImage2(
		core.NewQRectF4(
			0,
			0,
			w.devicePixelRatio*width,
			w.devicePixelRatio*float64(w.s.ws.font.lineHeight),
		).Size().ToSize(),
		gui.QImage__Format_ARGB32_Premultiplied,
	)
	image.SetDevicePixelRatio(w.devicePixelRatio)
	image.Fill3(core.Qt__transparent)

	pi := gui.NewQPainter2(image)
	pi.SetPen2(fg.QColor())

	pi.SetFont(font.fontNew)
	if highlight.bold {
		pi.Font().SetBold(true)
	}
	if highlight.italic {
		pi.Font().SetItalic(true)
	}

	pi.DrawText6(
		core.NewQRectF4(
			0,
			0,
			width,
			float64(w.s.ws.font.lineHeight),
		),
		text,
		gui.NewQTextOption2(core.Qt__AlignVCenter),
	)

	if w.font != nil {
		// If window has own font setting
		w.textCache.Set(
			HlChars{
				text:   text,
				fg:     highlight.fg(),
				italic: highlight.italic,
				bold:   highlight.bold,
			},
			image,
		)
	} else {
		// screen text cache
		w.s.textCache.Set(
			HlChars{
				text:   text,
				fg:     highlight.fg(),
				italic: highlight.italic,
				bold:   highlight.bold,
			},
			image,
		)
	}

	pi.DestroyQPainter()
	return image
}

// func (w *Window) drawChars(p *gui.QPainter, y int, col int, cols int) {
// 	if y >= len(w.content) {
// 		return
// 	}
// 	wsfont := w.getFont()
// 	specialChars := []int{}
// 	glyphMap := w.getGlyphMap()
//
// 	for x := col; x < col+cols; x++ {
// 		if x >= len(w.content[y]) {
// 			continue
// 		}
//
// 		cell := w.content[y][x]
// 		if cell == nil {
// 			continue
// 		}
// 		if cell.char == "" {
// 			continue
// 		}
// 		if !cell.normalWidth {
// 			specialChars = append(specialChars, x)
// 			continue
// 		}
// 		if cell.char == " " {
// 			continue
// 		}
//
// 		glyph, ok := glyphMap[HlChar{
// 			char:   cell.char,
// 			fg:     cell.highlight.fg(),
// 			bg:     cell.highlight.bg(),
// 			italic: cell.highlight.italic,
// 			bold:   cell.highlight.bold,
// 		}]
// 		if !ok {
// 			glyph = w.newGlyph(p, cell)
// 		} else {
// 		}
// 		p.DrawImage7(
// 			core.NewQPointF3(
// 				float64(x)*wsfont.truewidth,
// 				float64(y*wsfont.lineHeight),
// 			),
// 			&glyph,
// 		)
// 	}
//
// 	for _, x := range specialChars {
// 		cell := w.content[y][x]
// 		if cell == nil || cell.char == " " {
// 			continue
// 		}
// 		glyph, ok := glyphMap[HlChar{
// 			char:   cell.char,
// 			fg:     cell.highlight.fg(),
// 			bg:     cell.highlight.bg(),
// 			italic: cell.highlight.italic,
// 			bold:   cell.highlight.bold,
// 		}]
// 		if !ok {
// 			glyph = w.newGlyph(p, cell)
// 		}
// 		p.DrawImage7(
// 			core.NewQPointF3(
// 				float64(x)*wsfont.truewidth,
// 				float64(y*wsfont.lineHeight),
// 			),
// 			&glyph,
// 		)
// 	}
// }

// func (w *Window) newGlyph(p *gui.QPainter, cell *Cell) gui.QImage {
// 	// * TODO: Further optimization, whether it is possible
// 	// * Ref: https://stackoverflow.com/questions/40458515/a-best-way-to-draw-a-lot-of-independent-characters-in-qt5/40476430#40476430
//
// 	font := w.getFont()
// 	width := font.italicWidth
// 	if !cell.normalWidth {
// 		width = math.Ceil(font.fontMetrics.HorizontalAdvance(cell.char, -1))
// 	}
//
// 	char := cell.char
//
// 	// // If drawing background
// 	// if cell.highlight.background == nil {
// 	// 	cell.highlight.background = w.s.ws.background
// 	// }
// 	fg := cell.highlight.fg()
//
// 	// QImage default device pixel ratio is 1.0,
// 	// So we set the correct device pixel ratio
// 	glyph := gui.NewQImage2(
// 		// core.NewQRectF4(
// 		// 	0,
// 		// 	0,
// 		// 	w.devicePixelRatio*width,
// 		// 	w.devicePixelRatio*float64(font.lineHeight),
// 		// ).Size().ToSize(),
// 		core.NewQSize2(
// 			int(w.devicePixelRatio*width),
// 			int(w.devicePixelRatio*float64(font.lineHeight)),
// 		),
// 		gui.QImage__Format_ARGB32_Premultiplied,
// 	)
// 	glyph.SetDevicePixelRatio(w.devicePixelRatio)
//
// 	// // If drawing background
// 	// glyph.Fill2(gui.NewQColor3(
// 	// 	cell.highlight.background.R,
// 	// 	cell.highlight.background.G,
// 	// 	cell.highlight.background.B,
// 	// 	int(editor.config.Editor.Transparent * 255),
// 	// ))
// 	glyph.Fill3(core.Qt__transparent)
//
// 	p = gui.NewQPainter2(glyph)
// 	p.SetPen2(fg.QColor())
//
// 	p.SetFont(font.fontNew)
// 	if cell.highlight.bold {
// 		p.Font().SetBold(true)
// 	}
// 	if cell.highlight.italic {
// 		p.Font().SetItalic(true)
// 	}
//
// 	p.DrawText6(
// 		core.NewQRectF4(
// 			0,
// 			0,
// 			width,
// 			float64(font.lineHeight),
// 		),
// 		char,
// 		gui.NewQTextOption2(core.Qt__AlignVCenter),
// 	)
//
// 	if w.font != nil {
// 		w.glyphMap[HlChar{
// 			char:   cell.char,
// 			fg:     fg,
// 			bg:     cell.highlight.bg(),
// 			italic: cell.highlight.italic,
// 			bold:   cell.highlight.bold,
// 		}] = *glyph
//
// 	} else {
// 		w.s.glyphMap[HlChar{
// 			char:   cell.char,
// 			fg:     fg,
// 			bg:     cell.highlight.bg(),
// 			italic: cell.highlight.italic,
// 			bold:   cell.highlight.bold,
// 		}] = *glyph
// 	}
//
// 	return *glyph
// }

func (w *Window) drawText(p *gui.QPainter, y int, col int, cols int) {
	if y >= len(w.content) {
		return
	}
	wsfont := w.getFont()
	font := p.Font()
	line := w.content[y]
	chars := map[Highlight][]int{}
	specialChars := []int{}

	for x := col; x < col+cols; x++ {
		if x >= len(line) {
			continue
		}
		if line[x] == nil {
			continue
		}
		if line[x].char == " " {
			continue
		}
		if line[x].char == "" {
			continue
		}
		if !line[x].normalWidth {
			specialChars = append(specialChars, x)
			continue
		}

		highlight := line[x].highlight
		colorSlice, ok := chars[highlight]
		if !ok {
			colorSlice = []int{}
		}
		colorSlice = append(colorSlice, x)
		chars[highlight] = colorSlice
	}

	pointF := core.NewQPointF3(
		float64(col)*wsfont.truewidth,
		float64((y)*wsfont.lineHeight+wsfont.shift),
	)

	for highlight, colorSlice := range chars {
		var buffer bytes.Buffer
		slice := colorSlice[:]
		for x := col; x < col+cols; x++ {
			if len(slice) == 0 {
				break
			}
			index := slice[0]
			if x < index {
				buffer.WriteString(" ")
				continue
			}
			if x == index {
				buffer.WriteString(line[x].char)
				slice = slice[1:]
			}
		}

		text := buffer.String()
		if text != "" {
			fg := highlight.fg()
			if fg != nil {
				p.SetPen2(fg.QColor())
			}
			font.SetBold(highlight.bold)
			font.SetItalic(highlight.italic)
			p.DrawText(pointF, text)
		}

	}

	for _, x := range specialChars {
		if line[x] == nil || line[x].char == " " {
			continue
		}
		fg := line[x].highlight.fg()
		p.SetPen2(fg.QColor())
		pointF.SetX(float64(x) * wsfont.truewidth)
		pointF.SetY(float64((y)*wsfont.lineHeight + wsfont.shift))
		font.SetBold(line[x].highlight.bold)
		font.SetItalic(line[x].highlight.italic)
		p.DrawText(pointF, line[x].char)
	}
}

func (w *Window) drawContents(p *gui.QPainter, y int, col int, cols int) {
	if w.s.name == "minimap" {
		w.drawMinimap(p, y, col, cols)
	} else if !editor.config.Editor.CachedDrawing {
		w.drawText(p, y, col, cols)
	} else {
		// w.drawChars(p, y, col, cols)
		w.drawTextWithCache(p, y, col, cols)
	}
}

func (w *Window) drawTextDecoration(p *gui.QPainter, y int, col int, cols int) {
	if y >= len(w.content) {
		return
	}
	line := w.content[y]
	lenLine := w.lenLine[y]
	font := w.getFont()
	for x := col; x < col+cols; x++ {
		if x > lenLine {
			break
		}
		if x >= len(line) {
			continue
		}
		if line[x] == nil {
			continue
		}
		if !line[x].highlight.underline && !line[x].highlight.undercurl && !line[x].highlight.strikethrough {
			continue
		}
		pen := gui.NewQPen()
		sp := line[x].highlight.special
		if sp != nil {
			color := sp.QColor()
			pen.SetColor(color)
		} else {
			fg := editor.colors.fg
			color := fg.QColor()
			pen.SetColor(color)
		}
		p.SetPen(pen)
		start := float64(x) * font.truewidth
		end := float64(x+1) * font.truewidth
		Y := float64((y)*font.lineHeight) + font.ascent + float64(font.lineSpace)
		halfY := float64(y-1)*float64(font.lineHeight) + float64(font.lineHeight)/2.0 + font.ascent + float64(font.ascent)/2.0 + float64(font.lineSpace)
		if line[x].highlight.strikethrough {
			strikeLinef := core.NewQLineF3(start, halfY, end, halfY)
			p.DrawLine(strikeLinef)
		}
		if line[x].highlight.underline {
			linef := core.NewQLineF3(start, Y, end, Y)
			p.DrawLine(linef)
		}
		if line[x].highlight.undercurl {
			height := font.ascent / 3.0
			amplitude := font.ascent / 8.0
			freq := 1.0
			phase := 0.0
			y := Y + height/2 + amplitude*math.Sin(0)
			point := core.NewQPointF3(start, y)
			path := gui.NewQPainterPath2(point)
			for i := int(point.X()); i <= int(end); i++ {
				y = Y + height/2 + amplitude*math.Sin(2*math.Pi*freq*float64(i)/font.truewidth+phase)
				path.LineTo(core.NewQPointF3(float64(i), y))
			}
			p.DrawPath(path)
		}
	}
}

func getFillpatternAndTransparent(hl *Highlight) (core.Qt__BrushStyle, *RGBA, int) {
	color := hl.bg()
	pattern := core.Qt__BrushStyle(1)
	transparent := int(transparent() * 255.0)

	if editor.config.Editor.DiffChangePattern != 1 && hl.hlName == "DiffChange" {
		pattern = core.Qt__BrushStyle(editor.config.Editor.DiffChangePattern)
		if editor.config.Editor.DiffChangePattern >= 7 &&
			editor.config.Editor.DiffChangePattern <= 14 {
			transparent = int(editor.config.Editor.Transparent * 255)
		}
		color = color.HSV().Colorfulness().RGB()
	} else if editor.config.Editor.DiffDeletePattern != 1 && hl.hlName == "DiffDelete" {
		pattern = core.Qt__BrushStyle(editor.config.Editor.DiffDeletePattern)
		if editor.config.Editor.DiffDeletePattern >= 7 &&
			editor.config.Editor.DiffDeletePattern <= 14 {
			transparent = int(editor.config.Editor.Transparent * 255)
		}
		color = color.HSV().Colorfulness().RGB()
	} else if editor.config.Editor.DiffAddPattern != 1 && hl.hlName == "DiffAdd" {
		pattern = core.Qt__BrushStyle(editor.config.Editor.DiffAddPattern)
		if editor.config.Editor.DiffAddPattern >= 7 &&
			editor.config.Editor.DiffAddPattern <= 14 {
			transparent = int(editor.config.Editor.Transparent * 255)
		}
		color = color.HSV().Colorfulness().RGB()
	}

	return pattern, color, transparent
}

func (w *Window) isNormalWidth(char string) bool {
	if len(char) == 0 {
		return true
	}
	if char[0] <= 127 {
		return true
	}
	font := w.getFont()
	return font.fontMetrics.HorizontalAdvance(char, -1) == font.truewidth
}

func (s *Screen) windowPosition(args []interface{}) {
	for _, arg := range args {
		gridid := util.ReflectToInt(arg.([]interface{})[0])
		id := arg.([]interface{})[1].(nvim.Window)
		row := util.ReflectToInt(arg.([]interface{})[2])
		col := util.ReflectToInt(arg.([]interface{})[3])

		if isSkipGlobalId(gridid) {
			continue
		}

		win := s.windows[gridid]
		if win == nil {
			continue
		}

		win.id = id
		win.pos[0] = col
		win.pos[1] = row
		win.move(col, row)
		win.hideOverlappingWindows()
		win.show()
	}

	go s.setBufferNames()
}

func (s *Screen) setBufferNames() {
	for _, win := range s.ws.screen.windows {
		if win == nil {
			continue
		}
		if win.grid == 1 {
			continue
		}
		if win.isMsgGrid {
			continue
		}
		buf, _ := s.ws.nvim.WindowBuffer(win.id)
		bufName, _ := s.ws.nvim.BufferName(buf)
		win.bufName = bufName
	}
}

func (s *Screen) gridDestroy(args []interface{}) {
	for _, arg := range args {
		gridid := util.ReflectToInt(arg.([]interface{})[0])
		if isSkipGlobalId(gridid) {
			continue
		}
		if s.windows[gridid] == nil {
			continue
		}
		s.windows[gridid].hide()
		s.windows[gridid] = nil
	}
}

func (s *Screen) windowFloatPosition(args []interface{}) {
	for _, arg := range args {
		gridid := util.ReflectToInt(arg.([]interface{})[0])
		if isSkipGlobalId(gridid) {
			continue
		}
		s.windows[gridid].id = arg.([]interface{})[1].(nvim.Window)
		s.windows[gridid].anchor = arg.([]interface{})[2].(string)
		anchorGrid := util.ReflectToInt(arg.([]interface{})[3])
		anchorRow := int(util.ReflectToFloat(arg.([]interface{})[4]))
		anchorCol := int(util.ReflectToFloat(arg.([]interface{})[5]))
		// focusable := arg.([]interface{})[6]

		s.windows[gridid].widget.SetParent(editor.wsWidget)

		s.windows[gridid].pos[0] = anchorCol
		s.windows[gridid].pos[1] = anchorRow
		s.windows[gridid].isFloatWin = true

		x := s.windows[anchorGrid].pos[0] + s.windows[gridid].pos[0]
		y := s.windows[anchorGrid].pos[1] + s.windows[gridid].pos[1]
		s.windows[gridid].move(x, y)
		s.windows[gridid].setShadow()

		s.windows[gridid].show()
	}
}

func (s *Screen) windowHide(args []interface{}) {
	for _, arg := range args {
		gridid := util.ReflectToInt(arg.([]interface{})[0])
		if isSkipGlobalId(gridid) {
			continue
		}
		if s.windows[gridid] == nil {
			continue
		}
		s.windows[gridid].hide()
	}
}

// func (s *Screen) windowScrollOverStart() {
// 	// main purposs is to scroll over the windows in the `grid_line` event
// 	// when messages is shown
// 	s.isScrollOver = true
// }
//
// func (s *Screen) windowScrollOverReset() {
// 	s.isScrollOver = false
// 	s.scrollOverCount = 0
// 	for _, win := range s.windows {
// 		if win == nil {
// 			continue
// 		}
// 		if win.grid != 1 {
// 			win.move(win.pos[0], win.pos[1])
// 		}
// 	}
//
// 	// reset message contents in global grid
// 	gwin := s.windows[1]
// 	content := make([][]*Cell, gwin.rows)
// 	lenLine := make([]int, gwin.rows)
//
// 	for i := 0; i < gwin.rows; i++ {
// 		content[i] = make([]*Cell, gwin.cols)
// 	}
//
// 	for i := 0; i < gwin.rows; i++ {
// 		if i >= len(gwin.content) {
// 			continue
// 		}
// 		lenLine[i] = gwin.cols
// 		for j := 0; j < gwin.cols; j++ {
// 			if j >= len(gwin.content[i]) {
// 				continue
// 			}
// 			if gwin.content[i][j] == nil {
// 				continue
// 			}
// 			if gwin.content[i][j].highlight.hlName == "StatusLine" ||
// 				gwin.content[i][j].highlight.hlName == "StatusLineNC" ||
// 				gwin.content[i][j].highlight.hlName == "VertSplit" {
// 				content[i][j] = gwin.content[i][j]
// 			}
// 		}
// 	}
// 	s.windows[1].content = content
// 	s.windows[1].lenLine = lenLine
//
// 	gwin.queueRedrawAll()
// }

func (s *Screen) msgSetPos(args []interface{}) {
	for _, arg := range args {
		gridid := util.ReflectToInt(arg.([]interface{})[0])
		msgCount := util.ReflectToInt(arg.([]interface{})[1])
		win := s.windows[gridid]
		win.isMsgGrid = true
		win.pos[1] = msgCount
		win.move(win.pos[0], win.pos[1])
		win.show()
	}
}

func (s *Screen) windowClose() {
}

func (s *Screen) setColor() {
	s.tooltip.SetStyleSheet(
		fmt.Sprintf(
			" * {background-color: %s; text-decoration: underline; color: %s; }",
			editor.colors.selectedBg.String(),
			editor.colors.fg.String(),
		),
	)
}


func (w *Window) getCache() gcache.Cache {
       if w.font != nil {
               return w.textCache
       }

       return w.s.textCache
}

func newWindow() *Window {
	widget := widgets.NewQWidget(nil, 0)
	widget.SetContentsMargins(0, 0, 0, 0)
	widget.SetAttribute(core.Qt__WA_OpaquePaintEvent, true)
	widget.SetStyleSheet(" * { background-color: rgba(0, 0, 0, 0);}")

	var devicePixelRatio float64
	if runtime.GOOS == "darwin" {
		devicePixelRatio = 2.0
	} else {
		devicePixelRatio = 1.0
	}

	w := &Window{
		widget:           widget,
		scrollRegion:     []int{0, 0, 0, 0},
		devicePixelRatio: devicePixelRatio,
	}

	widget.ConnectPaintEvent(w.paint)

	return w
}

func (w *Window) isShown() bool {
	if w == nil {
		return false
	}
	if !w.shown {
		return false
	}

	return true
}

func (w *Window) raise() {
	if w.grid == 1 {
		return
	}
	w.widget.Raise()

	font := w.getFont()
	w.s.ws.cursor.updateFont(font)
	w.s.ws.cursor.widget.SetParent(w.widget)
	w.s.ws.cursor.widget.Hide()
	w.s.ws.cursor.widget.Show()

}

func (w *Window) hideOverlappingWindows() {
	if w.isMsgGrid {
		return
	}
	for _, win := range w.s.windows {
		if win == nil {
			continue
		}
		if win.grid == 1 {
			continue
		}
		if w == win {
			continue
		}
		if win.isMsgGrid {
			continue
		}
		if win.isFloatWin {
			continue
		}
		if !win.shown {
			continue
		}
		if w.widget.Geometry().Contains2(win.widget.Geometry(), false) {
			win.hide()
		}
	}
}

func (w *Window) show() {
	// w.hideOverlappingWindows()
	w.fill()
	w.widget.Show()
	w.shown = true
}

func (w *Window) hide() {
	w.widget.Hide()
	w.shown = false
}

func (w *Window) setParent(a widgets.QWidget_ITF) {
	w.widget.SetParent(a)
}

func (w *Window) setGeometryAndPalette(rect core.QRect_ITF) {
	w.widget.SetGeometry(rect)
	w.fill()
}

func (w *Window) fill() {
	if editor.config.Editor.DrawBorder {
		return
	}
	if w.s.ws.background != nil {
		w.widget.SetAutoFillBackground(true)
		p := gui.NewQPalette()
		p.SetColor2(gui.QPalette__Background, w.s.ws.background.QColor())
		w.widget.SetPalette(p)
	}
}

func (w *Window) setShadow() {
	w.widget.SetGraphicsEffect(util.DropShadow(0, 25, 125, 110))
}

func (w *Window) move(col int, row int) {
	res := 0
	// window position is based on cols, rows of global font setting
	// font := w.getFont()
	font := w.s.font
	if w.isMsgGrid {
		res = w.s.widget.Height() - w.rows*font.lineHeight
	}
	if res < 0 {
		res = 0
	}
	x := int(float64(col) * font.truewidth)
	y := row*int(font.lineHeight) + res
	if w.isFloatWin {
		if w.s.ws.drawTabline {
			y += 6 + w.s.ws.tabline.widget.Height()
		}
	}
	w.widget.Move2(x, y)

}

func isSkipGlobalId(id gridId) bool {
	if editor.config.Editor.SkipGlobalId {
		if id == 1 {
			return true
		} else {
			return false
		}
	} else {
		return false
	}
}
