package xiaomi

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	vp "miband/internal/pb/velapb"
	"miband/internal/pb/xiaomipb"
)

// fakeBand 模拟手环那一端：session、鉴权、ACK、通知、快应用列表/消息、装 rpk。
type fakeBand struct {
	t      *testing.T
	secret []byte
	onData func([]byte)

	mu          sync.Mutex
	buf         []byte
	seq         byte
	keys        *Keys // 手机侧视角的密钥；手环加密用 DecKey，解密用 EncKey
	phoneNonce  []byte
	watchNonce  []byte
	authed      bool
	notes       []string
	messages    []string
	statusSyncs int
	massParts   map[int][]byte
	massTotal   int
	massMD5     []byte
	installed   bool
	dropFirst   bool // 第一个数据帧不回 ACK，测试超时重发
}

func (b *fakeBand) Write(p []byte) error {
	b.mu.Lock()
	b.buf = append(b.buf, p...)
	var frames []frame
	for {
		f, n, ok := parseFrame(b.buf)
		if n == 0 {
			break
		}
		b.buf = b.buf[n:]
		if ok {
			frames = append(frames, f)
		}
	}
	b.mu.Unlock()
	for _, f := range frames {
		go b.handle(f) // 异步回复，模拟真实蓝牙
	}
	return nil
}

func (b *fakeBand) Close() {}

func (b *fakeBand) reply(kind byte, seq byte, payload []byte) {
	b.onData(encodeFrame(kind, seq, payload))
}

func (b *fakeBand) sendCmd(raw []byte, encrypt bool) {
	b.mu.Lock()
	seq := b.seq
	b.seq++
	body := []byte{chProtobuf, opPlain}
	if encrypt {
		body = append([]byte{chProtobuf, opEncrypt}, ctr(b.keys.DecKey, raw)...)
	} else {
		body = append(body, raw...)
	}
	b.mu.Unlock()
	b.reply(frameData, seq, body)
}

func (b *fakeBand) handle(f frame) {
	switch f.kind {
	case frameSessionConfig:
		b.reply(frameSessionConfig, 0, append([]byte{2}, sessionStart[1:]...))
		return
	case frameACK, frameNAK:
		return
	}
	b.mu.Lock()
	if b.dropFirst {
		b.dropFirst = false
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	b.reply(frameACK, f.seq, nil)

	channel, op, body := f.payload[0], f.payload[1], f.payload[2:]
	if channel == chMass {
		b.handleMass(body)
		return
	}
	if op == opEncrypt {
		body = ctr(b.keys.EncKey, body)
	}
	typ, id := peekTypeID(body)
	switch typ {
	case typeAuth:
		cmd := &xiaomipb.Command{}
		proto.Unmarshal(body, cmd)
		b.handleAuth(cmd)
	case typeNotification:
		cmd := &xiaomipb.Command{}
		proto.Unmarshal(body, cmd)
		n := cmd.GetNotification().GetNotification2().GetNotification3()
		b.mu.Lock()
		b.notes = append(b.notes, n.GetTitle()+"|"+n.GetBody())
		b.mu.Unlock()
	case typeThirdpartyApp, typeMass:
		pkt := &vp.VelaPacket{}
		proto.Unmarshal(body, pkt)
		b.handleVela(typ, id, pkt)
	}
}

func (b *fakeBand) handleAuth(cmd *xiaomipb.Command) {
	switch cmd.GetSubtype() {
	case authCmdNonce:
		b.mu.Lock()
		b.phoneNonce = cmd.GetAuth().GetPhoneNonce().GetNonce()
		b.watchNonce = make([]byte, 16)
		rand.Read(b.watchNonce)
		b.keys = newKeys(deriveKeys(b.secret, b.phoneNonce, b.watchNonce))
		hmacv := hmacSHA256(b.keys.DecKey, append(append([]byte{}, b.watchNonce...), b.phoneNonce...))
		b.mu.Unlock()
		raw, _ := proto.Marshal(&xiaomipb.Command{Type: proto.Uint32(typeAuth), Subtype: proto.Uint32(authCmdNonce),
			Auth: &xiaomipb.Auth{WatchNonce: &xiaomipb.WatchNonce{Nonce: b.watchNonce, Hmac: hmacv}}})
		b.sendCmd(raw, false)
	case authCmdAuth:
		step3 := cmd.GetAuth().GetAuthStep3()
		want := hmacSHA256(b.keys.EncKey, append(append([]byte{}, b.phoneNonce...), b.watchNonce...))
		if !bytes.Equal(step3.GetEncryptedNonces(), want) {
			b.t.Errorf("encryptedNonces 不对")
		}
		nonce := append(append([]byte{}, b.keys.EncNonce...), 0, 0, 0, 0, 0, 0, 0, 0)
		plain, err := ccmOpen(b.keys.EncKey, nonce, step3.GetEncryptedDeviceInfo(), 4)
		if err != nil {
			b.t.Errorf("设备信息解不开: %v", err)
		}
		info := &xiaomipb.AuthDeviceInfo{}
		if err := proto.Unmarshal(plain, info); err != nil || info.GetPhoneName() != "Mac" {
			b.t.Errorf("设备信息不对: %v %v", err, info)
		}
		b.mu.Lock()
		b.authed = true
		b.mu.Unlock()
		raw, _ := proto.Marshal(&xiaomipb.Command{Type: proto.Uint32(typeAuth), Subtype: proto.Uint32(authCmdAuth)})
		b.sendCmd(raw, true) // 鉴权成功的回包是加密的
	}
}

const fakePkg = "dev.test.app"

var fakeFingerprint = []byte("0123456789abcdefghij")

func (b *fakeBand) handleVela(typ, id uint32, pkt *vp.VelaPacket) {
	reply := func(p *vp.VelaPacket) {
		raw, _ := proto.Marshal(p)
		b.sendCmd(raw, true)
	}
	if typ == typeMass {
		pr := pkt.GetMass().GetPrepareRequest()
		b.mu.Lock()
		b.massMD5 = pr.GetDataId()
		b.massParts = map[int][]byte{}
		b.mu.Unlock()
		reply(&vp.VelaPacket{Type: proto.Uint32(typeMass), Id: proto.Uint32(id), Mass: &vp.Mass{Payload: &vp.Mass_PrepareResponse{
			PrepareResponse: &vp.PrepareResponse{PrepareStatus: vp.PrepareStatus_READY.Enum(), ExpectedSliceLength: proto.Uint32(100)}}}})
		return
	}
	resp := &vp.VelaPacket{Type: proto.Uint32(typ), Id: proto.Uint32(id), ThirdpartyApp: &vp.ThirdpartyApp{}}
	switch vp.ThirdpartyApp_ThirdpartyAppID(id) {
	case vp.ThirdpartyApp_GET_INSTALLED_LIST:
		resp.ThirdpartyApp.Payload = &vp.ThirdpartyApp_AppItemList{AppItemList: &vp.AppItemList{List: []*vp.AppItem{
			{PackageName: proto.String(fakePkg), Fingerprint: fakeFingerprint, VersionCode: proto.Uint32(1), AppName: proto.String("Test")},
		}}}
		reply(resp)
	case vp.ThirdpartyApp_PREPARE_INSTALL_APP:
		if pkt.GetThirdpartyApp().GetInstallRequest().GetPackageName() != fakePkg {
			b.t.Errorf("安装包名不对: %v", pkt)
		}
		resp.ThirdpartyApp.Payload = &vp.ThirdpartyApp_InstallResponse{InstallResponse: &vp.AppInstallResponse{PrepareStatus: vp.PrepareStatus_READY.Enum()}}
		reply(resp)
	case vp.ThirdpartyApp_SYNC_PHONE_APP_STATUS:
		st := pkt.GetThirdpartyApp().GetAppStatus()
		if !bytes.Equal(st.GetBasicInfo().GetFingerprint(), fakeFingerprint) || st.GetStatus() != vp.PhoneAppStatus_CONNECTED {
			b.t.Errorf("在线状态不对: %v", st)
		}
		b.mu.Lock()
		b.statusSyncs++
		b.mu.Unlock()
	case vp.ThirdpartyApp_SEND_PHONE_MESSAGE:
		mc := pkt.GetThirdpartyApp().GetMessageContent()
		if !bytes.Equal(mc.GetBasicInfo().GetFingerprint(), fakeFingerprint) {
			b.t.Errorf("消息指纹不对")
		}
		b.mu.Lock()
		b.messages = append(b.messages, string(mc.GetContent()))
		b.mu.Unlock()
	}
}

func (b *fakeBand) handleMass(body []byte) {
	total := int(binary.LittleEndian.Uint16(body[0:]))
	part := int(binary.LittleEndian.Uint16(body[2:]))
	b.mu.Lock()
	b.massTotal = total
	b.massParts[part] = append([]byte{}, body[4:]...)
	done := len(b.massParts) == total
	b.mu.Unlock()
	if !done {
		return
	}
	var inner []byte
	for i := 1; i <= total; i++ {
		inner = append(inner, b.massParts[i]...)
	}
	n := len(inner)
	if crc32.ChecksumIEEE(inner[:n-4]) != binary.LittleEndian.Uint32(inner[n-4:]) {
		b.t.Errorf("mass crc32 不对")
	}
	data := inner[22 : n-4]
	sum := md5.Sum(data)
	if inner[1] != massDataTypeApp || !bytes.Equal(inner[2:18], sum[:]) || !bytes.Equal(b.massMD5, sum[:]) ||
		binary.LittleEndian.Uint32(inner[18:]) != uint32(len(data)) {
		b.t.Errorf("mass 头不对")
	}
	b.mu.Lock()
	b.installed = true
	b.mu.Unlock()
	raw, _ := proto.Marshal(&vp.VelaPacket{Type: proto.Uint32(typeThirdpartyApp), Id: proto.Uint32(uint32(vp.ThirdpartyApp_REPORT_INSTALL_RESULT)),
		ThirdpartyApp: &vp.ThirdpartyApp{Payload: &vp.ThirdpartyApp_InstallResult{InstallResult: &vp.AppInstallResult{Code: vp.AppInstallResult_INSTALL_SUCCESS.Enum()}}}})
	b.sendCmd(raw, true)
}

// 快应用→手机：模拟快应用打开时问状态、发 hello
func (b *fakeBand) appSaysHello() {
	ask, _ := proto.Marshal(&vp.VelaPacket{Type: proto.Uint32(typeThirdpartyApp), Id: proto.Uint32(uint32(vp.ThirdpartyApp_REQUEST_PHONE_APP_STATUS)),
		ThirdpartyApp: &vp.ThirdpartyApp{Payload: &vp.ThirdpartyApp_BasicInfo{BasicInfo: &vp.BasicInfo{PackageName: proto.String(fakePkg), Fingerprint: fakeFingerprint}}}})
	b.sendCmd(ask, true)
	hello, _ := proto.Marshal(&vp.VelaPacket{Type: proto.Uint32(typeThirdpartyApp), Id: proto.Uint32(uint32(vp.ThirdpartyApp_SEND_WEAR_MESSAGE)),
		ThirdpartyApp: &vp.ThirdpartyApp{Payload: &vp.ThirdpartyApp_MessageContent{MessageContent: &vp.MessageContent{
			BasicInfo: &vp.BasicInfo{PackageName: proto.String(fakePkg), Fingerprint: fakeFingerprint}, Content: []byte(`{"t":"hello"}`)}}}})
	b.sendCmd(hello, true)
}

func newFake(t *testing.T, key string) (*Device, *fakeBand) {
	secret := make([]byte, 16)
	copy(secret, h(key))
	band := &fakeBand{t: t, secret: secret}
	dev, err := NewDevice("0x"+key, "aa:bb", func(addr string, onData func([]byte), onClosed func()) (Transport, error) {
		band.onData = onData
		return band, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dev, band
}

func TestConnectNotify(t *testing.T) {
	dev, band := newFake(t, "00112233445566778899aabbccddeeff")
	band.dropFirst = false
	if err := dev.Connect(); err != nil {
		t.Fatal(err)
	}
	if !dev.Connected() {
		t.Fatal("应该已连接")
	}
	if err := dev.Notify("claude 完成", "miband · 测试"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // 模拟手环和真手环一样先回 ACK 再处理
	band.mu.Lock()
	defer band.mu.Unlock()
	if len(band.notes) != 1 || band.notes[0] != "claude 完成|miband · 测试" {
		t.Fatalf("通知没收到: %v", band.notes)
	}
}

func TestWrongKey(t *testing.T) {
	dev, band := newFake(t, "00112233445566778899aabbccddeeff")
	band.secret = bytes.Repeat([]byte{0xff}, 16)
	if err := dev.Connect(); err == nil {
		t.Fatal("key 不对时应该鉴权失败")
	}
}

func TestRetransmitOnMissingAck(t *testing.T) {
	dev, band := newFake(t, "00112233445566778899aabbccddeeff")
	if err := dev.Connect(); err != nil {
		t.Fatal(err)
	}
	band.mu.Lock()
	band.dropFirst = true
	band.mu.Unlock()
	start := time.Now()
	if err := dev.SendData([]byte{chProtobuf, opPlain}, true, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("应该等了一次超时再重发")
	}
}

func TestAppsInstallAndMessage(t *testing.T) {
	dev, band := newFake(t, "00112233445566778899aabbccddeeff")
	apps := NewApps(dev)
	got := make(chan any, 1)
	apps.OnMessage = func(pkg string, payload any) { got <- payload }
	if err := dev.Connect(); err != nil {
		t.Fatal(err)
	}

	// 造一个 rpk：zip，根目录有 manifest.json，再塞点内容让它分成多片
	path := filepath.Join(t.TempDir(), "test.rpk")
	f, _ := os.Create(path)
	z := zip.NewWriter(f)
	w, _ := z.Create("manifest.json")
	w.Write([]byte(`{"package":"` + fakePkg + `","versionCode":3}`))
	w, _ = z.CreateHeader(&zip.FileHeader{Name: "blob.bin", Method: zip.Store})
	blob := make([]byte, 1000)
	rand.Read(blob)
	w.Write(blob)
	z.Close()
	f.Close()

	parts := 0
	m, err := apps.Install(path, func(i, n int) { parts = n })
	if err != nil {
		t.Fatal(err)
	}
	if m.Package != fakePkg || parts < 10 {
		t.Fatalf("安装结果不对: %v parts=%d", m, parts)
	}

	if err := apps.SetStatus(fakePkg, true); err != nil {
		t.Fatal(err)
	}
	if err := apps.Send(fakePkg, map[string]any{"t": "agents", "agents": []any{}}); err != nil {
		t.Fatal(err)
	}
	band.appSaysHello()
	select {
	case p := <-got:
		if m, ok := p.(map[string]any); !ok || m["t"] != "hello" {
			t.Fatalf("hello 不对: %v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没收到快应用的 hello")
	}
	time.Sleep(100 * time.Millisecond) // 等自动回复的在线状态
	band.mu.Lock()
	defer band.mu.Unlock()
	if !band.installed || len(band.messages) != 1 || band.messages[0] != `{"agents":[],"t":"agents"}` || band.statusSyncs != 2 {
		t.Fatalf("状态不对: installed=%v messages=%v syncs=%d", band.installed, band.messages, band.statusSyncs)
	}
}

// lockedConn 模拟真的 bt.Open：一个进程同时只能开一条连接，上一条没 Close，新的就一直等
type lockedConn struct {
	*fakeBand
	once   sync.Once
	unlock func()
}

func (c *lockedConn) Close() { c.once.Do(c.unlock) }

// 手环走远、Mac 睡眠时连接被动断开，没人调 Close；之后重连不能卡住
func TestReconnectAfterPassiveClose(t *testing.T) {
	key := "00112233445566778899aabbccddeeff"
	secret := make([]byte, 16)
	copy(secret, h(key))
	band := &fakeBand{t: t, secret: secret}
	var one sync.Mutex
	var closed func()
	dev, err := NewDevice("0x"+key, "aa:bb", func(addr string, onData func([]byte), onClosed func()) (Transport, error) {
		one.Lock()
		band.onData, closed = onData, onClosed
		return &lockedConn{fakeBand: band, unlock: one.Unlock}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dev.Connect(); err != nil {
		t.Fatal(err)
	}
	closed() // 被动断开
	if dev.Connected() {
		t.Fatal("断开后不该是已连接")
	}
	done := make(chan error, 1)
	go func() { done <- dev.Connect() }()
	select {
	case err := <-done:
		if err != nil || !dev.Connected() {
			t.Fatalf("重连失败：%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("重连卡住了：上一条连接没关，新连接一直在等")
	}
}

func TestParseBattery(t *testing.T) {
	raw, _ := proto.Marshal(&xiaomipb.Command{Type: proto.Uint32(typeSystem), Subtype: proto.Uint32(systemCmdBattery),
		System: &xiaomipb.System{Power: &xiaomipb.Power{Battery: &xiaomipb.Battery{Level: proto.Uint32(82), State: proto.Uint32(1)}}}})
	b, err := ParseBattery(raw)
	if err != nil || b != (Battery{Level: 82, Charging: true}) {
		t.Fatalf("got %+v, %v", b, err)
	}
	raw, _ = proto.Marshal(&xiaomipb.Command{Type: proto.Uint32(typeSystem), Subtype: proto.Uint32(systemCmdBattery)})
	if _, err := ParseBattery(raw); err == nil {
		t.Fatal("没电量字段应该报错")
	}
}
