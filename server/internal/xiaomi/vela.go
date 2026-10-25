package xiaomi

import (
	"archive/zip"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	vp "miband/internal/pb/velapb"
)

// 手环上的 Vela 快应用：列出/安装/启动，以及跟快应用互发消息（快应用侧是 @system.interconnect）。
//
// 协议参考 OronBox（lib/src/device/xiaomi/components/thirdparty_app_system.dart）：
//   - 命令 type 20（THIRDPARTY_APP），id 见 vela.proto 的 ThirdpartyAppID
//   - 手机→快应用：SEND_PHONE_MESSAGE；快应用→手机：SEND_WEAR_MESSAGE
//   - 消息里要带快应用的包名 + 签名指纹（指纹从 GET_INSTALLED_LIST 拿）
//   - 快应用打开 interconnect 时会来问手机端在不在，要回 SYNC_PHONE_APP_STATUS=CONNECTED，否则不触发 onopen
//
// 装 rpk：PREPARE_INSTALL_APP → MASS PREPARE → 分片（mass 通道明文，靠 v2 帧 ACK 确认）→ 等 REPORT_INSTALL_RESULT。

const (
	typeThirdpartyApp = 20
	typeMass          = 22
	massDataTypeApp   = 64
)

type Apps struct {
	dev *Device
	// 快应用发来的消息；payload 是 JSON 解析后的值（解析失败时是原始字符串）
	OnMessage func(pkg string, payload any)

	mu   sync.Mutex
	apps map[string]*vp.AppItem
}

func NewApps(dev *Device) *Apps {
	a := &Apps{dev: dev, apps: map[string]*vp.AppItem{}}
	dev.Handlers[typeThirdpartyApp] = a.onPacket
	return a
}

func packet(id vp.ThirdpartyApp_ThirdpartyAppID) *vp.VelaPacket {
	return &vp.VelaPacket{Type: proto.Uint32(typeThirdpartyApp), Id: proto.Uint32(uint32(id)), ThirdpartyApp: &vp.ThirdpartyApp{}}
}

func (a *Apps) send(pkt *vp.VelaPacket) error {
	data, err := proto.Marshal(pkt)
	if err != nil {
		return err
	}
	return a.dev.SendRaw(pkt.GetType(), data)
}

func (a *Apps) request(pkt *vp.VelaPacket, timeout time.Duration) (*vp.VelaPacket, error) {
	data, err := proto.Marshal(pkt)
	if err != nil {
		return nil, err
	}
	raw, err := a.dev.Request(pkt.GetType(), pkt.GetId(), data, pkt.GetId(), timeout)
	if err != nil {
		return nil, err
	}
	resp := &vp.VelaPacket{}
	return resp, proto.Unmarshal(raw, resp)
}

// List 列出手环上装的第三方快应用。
func (a *Apps) List() ([]*vp.AppItem, error) {
	resp, err := a.request(packet(vp.ThirdpartyApp_GET_INSTALLED_LIST), 10*time.Second)
	if err != nil {
		return nil, err
	}
	list := resp.GetThirdpartyApp().GetAppItemList().GetList()
	a.mu.Lock()
	a.apps = map[string]*vp.AppItem{}
	for _, it := range list {
		a.apps[it.GetPackageName()] = it
	}
	a.mu.Unlock()
	return list, nil
}

func (a *Apps) basic(pkg string) (*vp.BasicInfo, error) {
	a.mu.Lock()
	it := a.apps[pkg]
	a.mu.Unlock()
	if it == nil {
		if _, err := a.List(); err != nil {
			return nil, err
		}
		a.mu.Lock()
		it = a.apps[pkg]
		a.mu.Unlock()
	}
	if it == nil {
		return nil, fmt.Errorf("手环上没装 %s", pkg)
	}
	return &vp.BasicInfo{PackageName: proto.String(pkg), Fingerprint: it.GetFingerprint()}, nil
}

// SetStatus 告诉快应用“手机端”在线与否。
func (a *Apps) SetStatus(pkg string, connected bool) error {
	b, err := a.basic(pkg)
	if err != nil {
		return err
	}
	st := vp.PhoneAppStatus_DISCONNECTED
	if connected {
		st = vp.PhoneAppStatus_CONNECTED
	}
	pkt := packet(vp.ThirdpartyApp_SYNC_PHONE_APP_STATUS)
	pkt.ThirdpartyApp.Payload = &vp.ThirdpartyApp_AppStatus{AppStatus: &vp.PhoneAppStatus{BasicInfo: b, Status: st.Enum()}}
	return a.send(pkt)
}

// Send 发给快应用：非 []byte/string 的值会转成 JSON 字符串，快应用里 onmessage 的 data 就是这个字符串。
func (a *Apps) Send(pkg string, v any) error {
	var content []byte
	switch x := v.(type) {
	case []byte:
		content = x
	case string:
		content = []byte(x)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		content = b
	}
	b, err := a.basic(pkg)
	if err != nil {
		return err
	}
	pkt := packet(vp.ThirdpartyApp_SEND_PHONE_MESSAGE)
	pkt.ThirdpartyApp.Payload = &vp.ThirdpartyApp_MessageContent{MessageContent: &vp.MessageContent{BasicInfo: b, Content: content}}
	return a.send(pkt)
}

// Launch 打开快应用；uri 为空时打开入口页。
func (a *Apps) Launch(pkg, uri string) error {
	b, err := a.basic(pkg)
	if err != nil {
		return err
	}
	pkt := packet(vp.ThirdpartyApp_LAUNCH_APP)
	pkt.ThirdpartyApp.Payload = &vp.ThirdpartyApp_LaunchInfo{LaunchInfo: &vp.LaunchInfo{BasicInfo: b, Uri: proto.String(uri)}}
	return a.send(pkt)
}

func (a *Apps) Remove(pkg string) error {
	b, err := a.basic(pkg)
	if err != nil {
		return err
	}
	pkt := packet(vp.ThirdpartyApp_REMOVE_APP)
	pkt.ThirdpartyApp.Payload = &vp.ThirdpartyApp_BasicInfo{BasicInfo: b}
	a.mu.Lock()
	delete(a.apps, pkg)
	a.mu.Unlock()
	return a.send(pkt)
}

// Manifest 读 rpk（zip）根目录的 manifest.json。
type Manifest struct {
	Package     string `json:"package"`
	Name        string `json:"name"`
	VersionCode uint32 `json:"versionCode"`
}

func ReadManifest(path string) (*Manifest, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	f, err := z.Open("manifest.json")
	if err != nil {
		return nil, fmt.Errorf("rpk 里没有 manifest.json: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	m := &Manifest{}
	return m, json.Unmarshal(raw, m)
}

// Install 装 rpk；progress(i, n) 在每个分片被确认后调用。
func (a *Apps) Install(path string, progress func(i, n int)) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := ReadManifest(path)
	if err != nil {
		return nil, err
	}
	slog.Info("安装", "package", m.Package, "bytes", len(data))

	pkt := packet(vp.ThirdpartyApp_PREPARE_INSTALL_APP)
	pkt.ThirdpartyApp.Payload = &vp.ThirdpartyApp_InstallRequest{InstallRequest: &vp.AppInstallRequest{
		PackageName: proto.String(m.Package), VersionCode: proto.Uint32(max(m.VersionCode, 1)), PackageSize: proto.Uint32(uint32(len(data))),
	}}
	resp, err := a.request(pkt, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if st := resp.GetThirdpartyApp().GetInstallResponse().GetPrepareStatus(); st != vp.PrepareStatus_READY {
		return nil, fmt.Errorf("手环拒绝安装：%s", st)
	}

	result := a.dev.Expect(typeThirdpartyApp, uint32(vp.ThirdpartyApp_REPORT_INSTALL_RESULT)) // 先挂监听，结果可能来得很快
	defer a.dev.dropWaiter(result)
	if err := a.dev.massSend(massDataTypeApp, data, progress); err != nil {
		return nil, err
	}

	select {
	case raw := <-result:
		r := &vp.VelaPacket{}
		if err := proto.Unmarshal(raw, r); err != nil {
			return nil, err
		}
		if code := r.GetThirdpartyApp().GetInstallResult().GetCode(); code != vp.AppInstallResult_INSTALL_SUCCESS {
			return nil, fmt.Errorf("安装失败：%s", code)
		}
	case <-time.After(60 * time.Second):
		return nil, fmt.Errorf("等安装结果超时")
	}
	a.mu.Lock()
	delete(a.apps, m.Package)
	a.mu.Unlock()
	slog.Info("安装成功", "package", m.Package)
	return m, nil
}

func (a *Apps) onPacket(raw []byte) {
	pkt := &vp.VelaPacket{}
	if err := proto.Unmarshal(raw, pkt); err != nil {
		slog.Warn("解析快应用消息失败", "err", err)
		return
	}
	app := pkt.GetThirdpartyApp()
	switch {
	case pkt.GetId() == uint32(vp.ThirdpartyApp_SEND_WEAR_MESSAGE):
		mc := app.GetMessageContent()
		pkg := mc.GetBasicInfo().GetPackageName()
		var payload any
		if err := json.Unmarshal(mc.GetContent(), &payload); err != nil {
			payload = string(mc.GetContent())
		}
		slog.Debug("快应用发来消息", "package", pkg, "payload", payload)
		if a.OnMessage != nil {
			a.OnMessage(pkg, payload)
		}
	case app.GetBasicInfo() != nil:
		// 快应用打开 interconnect 时会来问手机端在不在；两个参考实现都只看 payload 类型不看 id
		pkg := app.GetBasicInfo().GetPackageName()
		slog.Debug("快应用询问手机端状态，回复在线", "package", pkg)
		go func() {
			if err := a.SetStatus(pkg, true); err != nil {
				slog.Warn("回复在线失败", "package", pkg, "err", err)
			}
		}()
	default:
		slog.Debug("快应用消息", "id", pkt.GetId())
	}
}

// massSend 走大文件通道把 data 传给手环（装快应用、装表盘）；progress(i, n) 在每个分片被确认后调用。
func (d *Device) massSend(dataType byte, data []byte, progress func(i, n int)) error {
	sum := md5.Sum(data)
	massPkt := &vp.VelaPacket{Type: proto.Uint32(typeMass), Id: proto.Uint32(uint32(vp.Mass_PREPARE)), Mass: &vp.Mass{
		Payload: &vp.Mass_PrepareRequest{PrepareRequest: &vp.PrepareRequest{
			DataType: proto.Uint32(uint32(dataType)), DataId: sum[:], DataLength: proto.Uint32(uint32(len(data))),
		}},
	}}
	raw, err := proto.Marshal(massPkt)
	if err != nil {
		return err
	}
	raw, err = d.Request(typeMass, uint32(vp.Mass_PREPARE), raw, uint32(vp.Mass_PREPARE), 10*time.Second)
	if err != nil {
		return err
	}
	resp := &vp.VelaPacket{}
	if err := proto.Unmarshal(raw, resp); err != nil {
		return err
	}
	mp := resp.GetMass().GetPrepareResponse()
	if mp.GetPrepareStatus() != vp.PrepareStatus_READY {
		return fmt.Errorf("手环拒绝接收文件：%s", mp.GetPrepareStatus())
	}
	if mp.GetRemainedDataLength() > 0 {
		slog.Warn("手环说已有部分数据（断点续传），这里不支持，仍发整个文件", "bytes", mp.GetRemainedDataLength())
	}
	sliceLen := int(mp.GetExpectedSliceLength())
	if sliceLen == 0 {
		sliceLen = 244
	}

	// 内层：00 | 类型 | md5 | 长度 | 文件 | crc32(前面全部)
	inner := append([]byte{0, dataType}, sum[:]...)
	inner = binary.LittleEndian.AppendUint32(inner, uint32(len(data)))
	inner = append(inner, data...)
	inner = binary.LittleEndian.AppendUint32(inner, crc32.ChecksumIEEE(inner))
	frag := sliceLen - 6
	total := (len(inner) + frag - 1) / frag
	for i := range total {
		// 分片走 mass 通道明文：02 01 | 总片数 | 序号(从 1 起) | 数据
		chunk := inner[i*frag : min((i+1)*frag, len(inner))]
		body := []byte{chMass, opPlain}
		body = binary.LittleEndian.AppendUint16(body, uint16(total))
		body = binary.LittleEndian.AppendUint16(body, uint16(i+1))
		body = append(body, chunk...)
		if err := d.SendData(body, true, 30*time.Second); err != nil {
			return err
		}
		if progress != nil {
			progress(i+1, total)
		}
	}
	return nil
}
