package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os/exec"
	"time"

	"github.com/getlantern/systray"

	"github.com/kiyoushii/EnvSync/internal/app"
)

// Иконка в трее: цвет показывает общее состояние, меню открывает интерфейс, ставит паузу и закрывает агента.
type trayLevel int

const (
	levelUnknown trayLevel = iota
	levelOK
	levelWarn
	levelBad
)

var levelColor = map[trayLevel]color.RGBA{
	levelUnknown: {0x8a, 0x93, 0xa5, 0xff},
	levelOK:      {0x1a, 0x9f, 0x5b, 0xff},
	levelWarn:    {0xe0, 0x9a, 0x1f, 0xff},
	levelBad:     {0xd0, 0x3e, 0x33, 0xff},
}

// makeIcon рисует круг заданного цвета и упаковывает PNG в формат ICO (его принимает Windows).
func makeIcon(c color.RGBA) []byte {
	const size = 32
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	cx, cy, r := float64(size)/2-0.5, float64(size)/2-0.5, float64(size)/2-2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)-cx, float64(y)-cy
			d := dx*dx + dy*dy
			switch {
			case d <= (r-4)*(r-4):
				img.SetRGBA(x, y, color.RGBA{0xff, 0xff, 0xff, 0xff}) // белая середина: читается и на тёмной панели
			case d <= r*r:
				img.SetRGBA(x, y, c)
			}
		}
	}
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)

	var ico bytes.Buffer
	le := binary.LittleEndian
	_ = binary.Write(&ico, le, [3]uint16{0, 1, 1}) // reserved, type=icon, count=1
	ico.Write([]byte{size, size, 0, 0})            // width, height, colors, reserved
	_ = binary.Write(&ico, le, [2]uint16{1, 32})   // planes, bits per pixel
	_ = binary.Write(&ico, le, uint32(pngBuf.Len()))
	_ = binary.Write(&ico, le, uint32(22)) // смещение данных: 6 + 16
	ico.Write(pngBuf.Bytes())
	return ico.Bytes()
}

func openBrowser(url string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// levelOf сводит статус машины в один цвет.
func levelOf(running bool, tsState string, paused bool, trips int, disconnected bool) (trayLevel, string) {
	switch {
	case trips > 0:
		return levelBad, fmt.Sprintf("нужно решение: остановлено массовое удаление (%d)", trips)
	case !running:
		return levelBad, "Syncthing не запущен"
	case tsState != "Running":
		return levelBad, "Tailscale не подключён"
	case paused:
		return levelWarn, "синхронизация на паузе"
	case disconnected:
		return levelWarn, "вторая машина недоступна"
	default:
		return levelOK, "работает"
	}
}

// runTray показывает значок и блокирует поток до выхода из меню или отмены ctx.
func runTray(ctx context.Context, quit func(), be *app.Backend, uiURL func() string) {
	systray.Run(func() {
		systray.SetIcon(makeIcon(levelColor[levelUnknown]))
		systray.SetTooltip("EnvSync")
		mOpen := systray.AddMenuItem("Открыть EnvSync", "Открыть интерфейс в браузере")
		mStatus := systray.AddMenuItem("Состояние: проверяю…", "")
		mStatus.Disable()
		mPause := systray.AddMenuItem("Пауза всего", "Приостановить синхронизацию со всеми устройствами")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Выход", "Остановить агента")

		go func() {
			var last trayLevel = -1
			paused := false
			refresh := func() {
				st, err := be.Self(ctx)
				if err != nil {
					return
				}
				disconnected := len(st.Syncthing.Devices) > 0
				for _, d := range st.Syncthing.Devices {
					if d.Connected {
						disconnected = false
					}
				}
				lvl, text := levelOf(st.Syncthing.Running, st.Tailscale.State, st.Paused, len(st.Guard), disconnected)
				if lvl != last {
					systray.SetIcon(makeIcon(levelColor[lvl]))
					last = lvl
				}
				paused = st.Paused
				systray.SetTooltip("EnvSync: " + text)
				mStatus.SetTitle("Состояние: " + text)
				if paused {
					mPause.SetTitle("Возобновить синхронизацию")
				} else {
					mPause.SetTitle("Пауза всего")
				}
			}
			refresh()
			tick := time.NewTicker(5 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					systray.Quit()
					return
				case <-tick.C:
					refresh()
				case <-mOpen.ClickedCh:
					openBrowser(uiURL())
				case <-mPause.ClickedCh:
					_ = be.SetPaused(ctx, !paused)
					refresh()
				case <-mQuit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}, quit)
}
