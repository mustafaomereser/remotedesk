//go:build windows

package capture

import (
	"log"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DXGI Desktop Duplication: masaüstü görüntüsünü GPU'dan, yalnızca değiştiğinde verir.
// GDI BitBlt 1080p'de ~20 ms sürer (≈48 fps tavanı); bu yol 1-3 ms sürer.
// Kullanılamazsa (eski sürücü, güvenli masaüstü, döndürülmüş ekran) GDI'a düşülür.

var (
	dxgiDLL  = windows.NewLazySystemDLL("dxgi.dll")
	d3d11DLL = windows.NewLazySystemDLL("d3d11.dll")

	procCreateDXGIFactory1 = dxgiDLL.NewProc("CreateDXGIFactory1")
	procD3D11CreateDevice  = d3d11DLL.NewProc("D3D11CreateDevice")

	iidFactory1  = windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	iidOutput1   = windows.GUID{Data1: 0x00cddea8, Data2: 0x939b, Data3: 0x4b83, Data4: [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidTexture2D = windows.GUID{Data1: 0x6f15aaf2, Data2: 0xd208, Data3: 0x4e89, Data4: [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

const (
	dxgiErrWaitTimeout = 0x887A0027
	dxgiFormatBGRA     = 87
)

// comCall, COM nesnesinin vtable'ındaki idx. metodu çağırır (ilk argüman nesnenin kendisi).
func comCall(obj uintptr, idx int, args ...uintptr) uintptr {
	p := *(***[64]uintptr)(unsafe.Pointer(&obj)) // nesne -> vtable işaretçisi -> metot tablosu
	fn := (**p)[idx]
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{obj}, args...)...)
	return r
}

func comRelease(obj uintptr) {
	if obj != 0 {
		comCall(obj, 2)
	}
}

func failed(hr uintptr) bool { return int32(hr) < 0 }

type dxgiOutputDesc struct {
	DeviceName [32]uint16
	Coords     rectW
	Attached   int32
	Rotation   uint32
	Monitor    uintptr
}

type dxgiFrameInfo struct {
	LastPresentTime     int64
	LastMouseUpdateTime int64
	AccumulatedFrames   uint32
	RectsCoalesced      int32
	ProtectedMasked     int32
	PointerX, PointerY  int32
	PointerVisible      int32
	TotalMetadataSize   uint32
	PointerShapeSize    uint32
}

type d3dTexture2DDesc struct {
	Width, Height, MipLevels, ArraySize uint32
	Format                              uint32
	SampleCount, SampleQuality          uint32
	Usage, BindFlags, CPUAccess, Misc   uint32
}

type d3dMapped struct {
	Data       unsafe.Pointer
	RowPitch   uint32
	DepthPitch uint32
}

type dxgiDup struct {
	display Display
	device  uintptr
	ctx     uintptr
	dup     uintptr
	staging uintptr
	have    bool // DIB'de geçerli bir kare var
}

func (x *dxgiDup) close() {
	comRelease(x.staging)
	comRelease(x.dup)
	comRelease(x.ctx)
	comRelease(x.device)
	*x = dxgiDup{}
}

// openDup, d monitörünün çıkışını bulup kopyalamayı başlatır.
func openDup(d Display) *dxgiDup {
	if procCreateDXGIFactory1.Find() != nil || procD3D11CreateDevice.Find() != nil {
		return nil
	}
	var factory uintptr
	if hr, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidFactory1)), uintptr(unsafe.Pointer(&factory))); failed(hr) || factory == 0 {
		return nil
	}
	defer comRelease(factory)

	for ai := uintptr(0); ; ai++ {
		var adapter uintptr
		if failed(comCall(factory, 7, ai, uintptr(unsafe.Pointer(&adapter)))) || adapter == 0 { // EnumAdapters
			return nil
		}
		for oi := uintptr(0); ; oi++ {
			var output uintptr
			if failed(comCall(adapter, 7, oi, uintptr(unsafe.Pointer(&output)))) || output == 0 { // EnumOutputs
				break
			}
			var desc dxgiOutputDesc
			comCall(output, 7, uintptr(unsafe.Pointer(&desc))) // GetDesc
			match := int(desc.Coords.Left) == d.X && int(desc.Coords.Top) == d.Y &&
				int(desc.Coords.Right-desc.Coords.Left) == d.W && int(desc.Coords.Bottom-desc.Coords.Top) == d.H
			if !match || desc.Rotation > 1 { // döndürülmüş ekran: GDI
				comRelease(output)
				continue
			}
			x := startDup(adapter, output)
			comRelease(output)
			comRelease(adapter)
			if x != nil {
				x.display = d
			}
			return x
		}
		comRelease(adapter)
	}
}

func startDup(adapter, output uintptr) *dxgiDup {
	x := &dxgiDup{}
	// D3D_DRIVER_TYPE_UNKNOWN (0): adaptör açıkça verildi; SDK sürümü 7
	hr, _, _ := procD3D11CreateDevice.Call(adapter, 0, 0, 0, 0, 0, 7,
		uintptr(unsafe.Pointer(&x.device)), 0, uintptr(unsafe.Pointer(&x.ctx)))
	if failed(hr) || x.device == 0 || x.ctx == 0 {
		x.close()
		return nil
	}
	var out1 uintptr
	if failed(comCall(output, 0, uintptr(unsafe.Pointer(&iidOutput1)), uintptr(unsafe.Pointer(&out1)))) || out1 == 0 {
		x.close()
		return nil
	}
	defer comRelease(out1)
	if hr := comCall(out1, 22, x.device, uintptr(unsafe.Pointer(&x.dup))); failed(hr) || x.dup == 0 { // DuplicateOutput
		log.Printf("capture: DXGI DuplicateOutput başarısız (0x%08X), GDI kullanılıyor", uint32(hr))
		x.close()
		return nil
	}
	return x
}

type dxgiResult int

const (
	dxgiFail dxgiResult = iota
	dxgiNew
	dxgiSame
)

// grab, yeni kareyi dst'ye (w×h BGRA, stride w*4) kopyalar. Ekran değişmediyse dxgiSame döner.
func (x *dxgiDup) grab(dst []byte, w, h int, wait time.Duration) dxgiResult {
	var info dxgiFrameInfo
	var res uintptr
	hr := comCall(x.dup, 8, uintptr(wait.Milliseconds()), uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&res))) // AcquireNextFrame
	if uint32(hr) == dxgiErrWaitTimeout {
		if x.have {
			return dxgiSame
		}
		return dxgiFail
	}
	if failed(hr) || res == 0 {
		return dxgiFail // erişim kaybı (çözünürlük, UAC, tam ekran geçişi): çağıran yeniden açar
	}
	defer comCall(x.dup, 14) // ReleaseFrame
	defer comRelease(res)

	var tex uintptr
	if failed(comCall(res, 0, uintptr(unsafe.Pointer(&iidTexture2D)), uintptr(unsafe.Pointer(&tex)))) || tex == 0 {
		return dxgiFail
	}
	defer comRelease(tex)

	if x.staging == 0 {
		desc := d3dTexture2DDesc{
			Width: uint32(w), Height: uint32(h), MipLevels: 1, ArraySize: 1,
			Format: dxgiFormatBGRA, SampleCount: 1,
			Usage: 3 /* STAGING */, CPUAccess: 0x20000, /* READ */
		}
		if failed(comCall(x.device, 5, uintptr(unsafe.Pointer(&desc)), 0, uintptr(unsafe.Pointer(&x.staging)))) || x.staging == 0 { // CreateTexture2D
			return dxgiFail
		}
	}
	comCall(x.ctx, 47, x.staging, tex) // CopyResource
	var m d3dMapped
	if failed(comCall(x.ctx, 14, x.staging, 0, 1 /* MAP_READ */, 0, uintptr(unsafe.Pointer(&m)))) || m.Data == nil { // Map
		return dxgiFail
	}
	row := w * 4
	pitch := int(m.RowPitch)
	if pitch < row {
		comCall(x.ctx, 15, x.staging, 0)
		return dxgiFail
	}
	src := unsafe.Slice((*byte)(m.Data), pitch*(h-1)+row)
	if pitch == row {
		copy(dst, src)
	} else {
		for y := 0; y < h; y++ {
			copy(dst[y*row:(y+1)*row], src[y*pitch:])
		}
	}
	comCall(x.ctx, 15, x.staging, 0) // Unmap
	x.have = true
	return dxgiNew
}
