package video

import (
	"runtime"
	"strconv"
)

// candidates, denenecek H.264 kodlayıcılarıdır (öncelik sırasıyla).
func candidates() []string {
	if runtime.GOOS == "darwin" {
		return []string{"h264_videotoolbox", "libx264"}
	}
	return []string{"h264_nvenc", "h264_qsv", "h264_amf", "libx264", "h264_mf"}
}

// encoderArgs, düşük gecikme için kodlayıcıya özel ayarlardır.
// B-frame yok (her giriş karesi hemen çıkar), anahtar kare aralığı uzun
// (ekran durağanken zaten kare üretilmiyor).
func encoderArgs(enc string, bitrate int) []string {
	b := strconv.Itoa(bitrate)
	half := strconv.Itoa(bitrate / 2)
	var a []string
	switch enc {
	case "libx264":
		a = []string{"-preset", "ultrafast", "-tune", "zerolatency", "-crf", "23", "-maxrate", b, "-bufsize", half}
	case "h264_nvenc":
		a = []string{"-preset", "p2", "-tune", "ull", "-zerolatency", "1", "-rc", "vbr", "-cq", "23", "-b:v", b, "-maxrate", b, "-bufsize", half}
	case "h264_qsv":
		a = []string{"-preset", "veryfast", "-async_depth", "1", "-b:v", b}
	case "h264_amf":
		a = []string{"-usage", "ultralowlatency", "-quality", "speed", "-rc", "vbr_peak", "-b:v", b, "-maxrate", b}
	case "h264_mf":
		a = []string{"-scenario", "display_remoting", "-b:v", b}
	case "h264_videotoolbox":
		a = []string{"-realtime", "1", "-prio_speed", "1", "-allow_sw", "1", "-b:v", b}
	default:
		a = []string{"-b:v", b}
	}
	return append(a, "-g", "600", "-bf", "0")
}

var rateLadder = []int{1_500_000, 2_500_000, 4_000_000, 6_000_000, 10_000_000, 20_000_000}

// StepRate, bit hızı basamaklarında bir aşağı (dir<0) veya yukarı (dir>0) değeri döner.
func StepRate(rate, dir int) int {
	if dir < 0 {
		for i := len(rateLadder) - 1; i >= 0; i-- {
			if rateLadder[i] < rate {
				return rateLadder[i]
			}
		}
		return rate
	}
	for _, r := range rateLadder {
		if r > rate {
			return r
		}
	}
	return rate
}

// BitrateFor, viewer'daki kalite ayarını (10-95) bit hızına çevirir.
func BitrateFor(quality int) int {
	switch {
	case quality <= 45:
		return 4_000_000
	case quality <= 70:
		return 10_000_000
	default:
		return 20_000_000
	}
}
