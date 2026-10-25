// Package xiaomi 实现小米 Vela 手环（实测手环 10 Pro）的连接协议：SPP v2 帧、鉴权、加密收发、通知、快应用。
//
// 协议移植自 Gadgetbridge（XiaomiAuthService / XiaomiSppProtocolV2）和 OronBox（快应用、大文件传输）。
package xiaomi

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"miband/internal/pb/xiaomipb"
)

const (
	typeAuth         = 1
	typeSystem       = 2
	typeNotification = 7

	authCmdNonce = 26
	authCmdAuth  = 27

	systemCmdBattery    = 1
	systemCmdClock      = 3
	notificationCmdSend = 0
)

// Transport 是底层字节流连接（macOS 上是 IOBluetooth 的 RFCOMM）。
type Transport interface {
	Write([]byte) error
	Close()
}

// Dialer 打开到手环的连接；onData/onClosed 可能在任意线程上被调用。
type Dialer func(address string, onData func([]byte), onClosed func()) (Transport, error)

type waiter struct {
	typ, id uint32
	ch      chan []byte
}

// Device 是一条到手环的会话。Connect 之后可以并发调用各个发送方法。
type Device struct {
	Address string
	// 收到某个命令 type 的包（未被 Request 认领时）交给这里，参数是原始 protobuf 字节
	Handlers     map[uint32]func([]byte)
	OnDisconnect func()
	// 手环主动报上来电量（比如插上、拔下充电器）
	OnBattery func(Battery)

	secret []byte
	dial   Dialer

	mu         sync.Mutex
	conn       Transport
	open       bool
	keys       *Keys
	authed     bool
	phoneNonce []byte
	authDone   chan error
	session    chan struct{}
	acks       map[byte]chan bool
	waiters    []*waiter
	rx         chan []byte

	sendMu sync.Mutex // 停等发送：同一时间只有一个数据帧在等 ACK
	seq    byte
}

func NewDevice(authKey, address string, dial Dialer) (*Device, error) {
	key, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(authKey), "0x"))
	if err != nil {
		return nil, fmt.Errorf("auth_key 不是合法的 hex: %w", err)
	}
	secret := make([]byte, 16)
	copy(secret, key)
	return &Device{Address: address, secret: secret, dial: dial, Handlers: map[uint32]func([]byte){}}, nil
}

func (d *Device) Connected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.open && d.authed
}

// Connect 打开连接 → 开 v2 session → 鉴权 → 对时。
func (d *Device) Connect() error {
	// 上一条连接被动断开时（手环走远、Mac 睡眠）没人调 Close，先关掉：
	// bt.Open 一次只开一条连接，上一条不关，新的会一直等，表现为一直「正在连接」也不报错
	d.Close()
	d.mu.Lock()
	d.keys, d.authed, d.seq = nil, false, 0
	d.acks = map[byte]chan bool{}
	d.waiters = nil
	d.session = make(chan struct{}, 1)
	d.rx = make(chan []byte, 256)
	rx := d.rx
	d.mu.Unlock()

	go d.readLoop(rx)
	conn, err := d.dial(d.Address, func(b []byte) {
		slog.Debug("收到", "bytes", len(b))
		rx <- b
	}, d.onClosed)
	if err != nil {
		close(rx)
		return err
	}
	d.mu.Lock()
	d.conn, d.open = conn, true
	d.mu.Unlock()
	slog.Info("已连接", "address", d.Address)

	// 手环 10 系列不回 v1 版本探测，直接开 v2 session（同 Gadgetbridge XiaomiBleProtocolV2）
	if err := d.write(encodeFrame(frameSessionConfig, 0, sessionStart)); err != nil {
		d.Close()
		return err
	}
	select {
	case <-d.session:
	case <-time.After(5 * time.Second):
		d.Close()
		return errors.New("手环没回 session 响应")
	}
	if err := d.authenticate(); err != nil {
		d.Close()
		return err
	}
	slog.Info("鉴权成功")
	return d.SetTime(time.Now())
}

func (d *Device) Close() {
	d.mu.Lock()
	conn := d.conn
	d.conn, d.open, d.authed = nil, false, false
	d.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

func (d *Device) onClosed() {
	slog.Warn("手环断开")
	d.mu.Lock()
	d.open, d.authed = false, false
	authDone := d.authDone
	d.mu.Unlock()
	if authDone != nil {
		select {
		case authDone <- errors.New("鉴权过程中手环断开"):
		default:
		}
	}
	if d.OnDisconnect != nil {
		d.OnDisconnect()
	}
}

func (d *Device) write(b []byte) error {
	d.mu.Lock()
	conn := d.conn
	d.mu.Unlock()
	if conn == nil {
		return errors.New("连接未打开")
	}
	return conn.Write(b)
}

// ---------- 鉴权 ----------

func (d *Device) authenticate() error {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	done := make(chan error, 1)
	d.mu.Lock()
	d.phoneNonce, d.authDone = nonce, done
	d.mu.Unlock()

	cmd := &xiaomipb.Command{
		Type:    proto.Uint32(typeAuth),
		Subtype: proto.Uint32(authCmdNonce),
		Auth:    &xiaomipb.Auth{PhoneNonce: &xiaomipb.PhoneNonce{Nonce: nonce}},
	}
	if err := d.SendCommand(cmd); err != nil {
		return err
	}
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		return errors.New("鉴权超时")
	}
}

func (d *Device) handleAuth(cmd *xiaomipb.Command) {
	d.mu.Lock()
	done, phoneNonce := d.authDone, d.phoneNonce
	d.mu.Unlock()
	finish := func(err error) {
		if done != nil {
			select {
			case done <- err:
			default:
			}
		}
	}

	switch cmd.GetSubtype() {
	case authCmdNonce:
		watch := cmd.GetAuth().GetWatchNonce()
		keys := newKeys(deriveKeys(d.secret, phoneNonce, watch.GetNonce()))
		if !bytes.Equal(hmacSHA256(keys.DecKey, append(append([]byte{}, watch.GetNonce()...), phoneNonce...)), watch.GetHmac()) {
			finish(errors.New("auth key 不对（手环返回的 hmac 校验失败）"))
			return
		}
		info, _ := proto.Marshal(&xiaomipb.AuthDeviceInfo{
			Unknown1: proto.Uint32(0), PhoneApiLevel: proto.Float32(34), PhoneName: proto.String("Mac"),
			Unknown3: proto.Uint32(224), Region: proto.String("CN"),
		})
		reply := &xiaomipb.Command{
			Type:    proto.Uint32(typeAuth),
			Subtype: proto.Uint32(authCmdAuth),
			Auth: &xiaomipb.Auth{AuthStep3: &xiaomipb.AuthStep3{
				EncryptedNonces:     hmacSHA256(keys.EncKey, append(append([]byte{}, phoneNonce...), watch.GetNonce()...)),
				EncryptedDeviceInfo: keys.EncryptCCM(info, 0),
			}},
		}
		// 解密密钥现在就要装上：手环回的“鉴权成功”是加密的
		d.mu.Lock()
		d.keys = keys
		d.mu.Unlock()
		go func() {
			if err := d.SendCommand(reply); err != nil {
				finish(err)
			}
		}()
	case authCmdAuth:
		d.mu.Lock()
		d.authed = true
		d.mu.Unlock()
		finish(nil)
	default:
		slog.Debug("忽略鉴权消息", "subtype", cmd.GetSubtype())
	}
}

// ---------- 发送 ----------

// SendCommand 发一个 protobuf 命令。
func (d *Device) SendCommand(cmd *xiaomipb.Command) error {
	data, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	return d.SendRaw(cmd.GetType(), data)
}

// SendRaw 发一个已序列化的 protobuf 包。鉴权命令走明文且不等 ACK，其它命令加密并等 ACK。
func (d *Device) SendRaw(typ uint32, data []byte) error {
	d.mu.Lock()
	keys, authed := d.keys, d.authed
	d.mu.Unlock()
	body := []byte{chProtobuf, opPlain}
	if typ != typeAuth && authed {
		body = append([]byte{chProtobuf, opEncrypt}, keys.EncryptCTR(data)...)
	} else {
		body = append(body, data...)
	}
	return d.SendData(body, typ != typeAuth, 5*time.Second)
}

// SendData 发一个 v2 数据帧并停等 ACK；NAK 或超时就原样重发。
// 不等 ACK 紧接着断开的话，手环来不及处理（实测通知会丢）。
func (d *Device) SendData(body []byte, waitAck bool, timeout time.Duration) error {
	d.sendMu.Lock()
	defer d.sendMu.Unlock()
	d.mu.Lock()
	seq := d.seq
	d.seq++
	d.mu.Unlock()
	f := encodeFrame(frameData, seq, body)
	if !waitAck {
		return d.write(f)
	}
	const retries = 3
	for range retries + 1 {
		ch := make(chan bool, 1)
		d.mu.Lock()
		d.acks[seq] = ch
		d.mu.Unlock()
		if err := d.write(f); err != nil {
			return err
		}
		select {
		case ok := <-ch:
			if ok {
				return nil
			}
			slog.Debug("收到 NAK，重发", "seq", seq)
		case <-time.After(timeout):
			slog.Debug("等 ACK 超时，重发", "seq", seq)
		}
		d.mu.Lock()
		delete(d.acks, seq)
		d.mu.Unlock()
	}
	return fmt.Errorf("数据帧 seq=%d 重发 %d 次仍没被确认", seq, retries)
}

// Expect 先登记要等的回包，再发请求，避免回包比等待先到。
func (d *Device) Expect(typ, id uint32) <-chan []byte {
	w := &waiter{typ: typ, id: id, ch: make(chan []byte, 1)}
	d.mu.Lock()
	d.waiters = append(d.waiters, w)
	d.mu.Unlock()
	return w.ch
}

func (d *Device) dropWaiter(ch <-chan []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, w := range d.waiters {
		if w.ch == ch {
			d.waiters = append(d.waiters[:i], d.waiters[i+1:]...)
			return
		}
	}
}

// Request 发包并等同 type、id 为 replyID 的回包，返回回包的原始字节。
func (d *Device) Request(typ, id uint32, data []byte, replyID uint32, timeout time.Duration) ([]byte, error) {
	ch := d.Expect(typ, replyID)
	defer d.dropWaiter(ch)
	if err := d.SendRaw(typ, data); err != nil {
		return nil, err
	}
	select {
	case b := <-ch:
		return b, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("等回包超时 (type=%d id=%d)", typ, replyID)
	}
}

// ---------- 接收 ----------

func (d *Device) readLoop(rx <-chan []byte) {
	var buf []byte
	for b := range rx {
		buf = append(buf, b...)
		for len(buf) > 0 {
			f, n, ok := parseFrame(buf)
			if n == 0 {
				break
			}
			buf = buf[n:]
			if ok {
				d.handleFrame(f)
			}
		}
	}
}

func (d *Device) handleFrame(f frame) {
	switch f.kind {
	case frameSessionConfig:
		select {
		case d.session <- struct{}{}:
		default:
		}
	case frameACK, frameNAK:
		// ACK(S)：S 及之前的都确认了（累计确认，序号 8 位回绕）；NAK(S)：S 之前确认了，S 及之后要重发
		d.mu.Lock()
		for seq, ch := range d.acks {
			before := byte(f.seq-seq) < 128
			var result, settle bool
			if f.kind == frameACK && before {
				result, settle = true, true
			} else if f.kind == frameNAK {
				result, settle = before && seq != f.seq, true
			}
			if settle {
				ch <- result
				delete(d.acks, seq)
			}
		}
		d.mu.Unlock()
	case frameData:
		go d.write(encodeFrame(frameACK, f.seq, nil))
		if len(f.payload) < 2 {
			return
		}
		channel, op, body := f.payload[0]&0xF, f.payload[1], f.payload[2:]
		if op == opEncrypt {
			d.mu.Lock()
			keys := d.keys
			d.mu.Unlock()
			if keys == nil {
				return
			}
			body = keys.DecryptCTR(body)
		}
		if channel == chProtobuf {
			d.dispatch(body)
		}
	}
}

// peekTypeID 只解析字段 1(type) 和 2(id)；不同 type 的负载用不同的 proto，先看类型再分发。
func peekTypeID(b []byte) (typ, id uint32) {
	for len(b) > 0 {
		num, wt, n := protowire.ConsumeTag(b)
		if n < 0 {
			return
		}
		b = b[n:]
		if wt == protowire.VarintType && (num == 1 || num == 2) {
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return
			}
			if num == 1 {
				typ = uint32(v)
			} else {
				id = uint32(v)
			}
			b = b[n:]
			continue
		}
		n = protowire.ConsumeFieldValue(num, wt, b)
		if n < 0 {
			return
		}
		b = b[n:]
	}
	return
}

func (d *Device) dispatch(body []byte) {
	typ, id := peekTypeID(body)
	d.mu.Lock()
	for i, w := range d.waiters {
		if w.typ == typ && w.id == id {
			d.waiters = append(d.waiters[:i], d.waiters[i+1:]...)
			d.mu.Unlock()
			w.ch <- body
			return
		}
	}
	handler := d.Handlers[typ]
	d.mu.Unlock()
	if handler != nil {
		handler(body)
		return
	}
	if typ == typeAuth {
		cmd := &xiaomipb.Command{}
		if err := proto.Unmarshal(body, cmd); err != nil {
			slog.Warn("解析鉴权消息失败", "err", err)
			return
		}
		d.handleAuth(cmd)
		return
	}
	if typ == typeSystem && id == systemCmdBattery && d.OnBattery != nil {
		if b, err := ParseBattery(body); err == nil {
			d.OnBattery(b)
		}
		return
	}
	slog.Debug("手环命令", "type", typ, "id", id)
}

// ---------- 常用命令 ----------

func (d *Device) SetTime(now time.Time) error {
	_, offset := now.Zone()
	name := now.Location().String()
	if name == "Local" {
		name = localZoneName()
	}
	cmd := &xiaomipb.Command{
		Type:    proto.Uint32(typeSystem),
		Subtype: proto.Uint32(systemCmdClock),
		System: &xiaomipb.System{Clock: &xiaomipb.Clock{
			Date: &xiaomipb.Date{Year: proto.Uint32(uint32(now.Year())), Month: proto.Uint32(uint32(now.Month())), Day: proto.Uint32(uint32(now.Day()))},
			Time: &xiaomipb.Time{Hour: proto.Uint32(uint32(now.Hour())), Minute: proto.Uint32(uint32(now.Minute())),
				Second: proto.Uint32(uint32(now.Second())), Millisecond: proto.Uint32(0)},
			Timezone:    &xiaomipb.TimeZone{ZoneOffset: proto.Int32(int32(offset / 60 / 15)), DstOffset: proto.Int32(0), Name: proto.String(name)},
			IsNot24Hour: proto.Bool(false),
		}},
	}
	return d.SendCommand(cmd)
}

// Battery 是手环的电量。
type Battery struct {
	Level    int  `json:"level"` // 0–100
	Charging bool `json:"charging"`
}

// Battery 读手环的电量。
func (d *Device) Battery() (Battery, error) {
	data, err := proto.Marshal(&xiaomipb.Command{Type: proto.Uint32(typeSystem), Subtype: proto.Uint32(systemCmdBattery)})
	if err != nil {
		return Battery{}, err
	}
	raw, err := d.Request(typeSystem, systemCmdBattery, data, systemCmdBattery, 5*time.Second)
	if err != nil {
		return Battery{}, err
	}
	return ParseBattery(raw)
}

// ParseBattery 解析手环回的（或主动报上来的）电量命令。
func ParseBattery(raw []byte) (Battery, error) {
	cmd := &xiaomipb.Command{}
	if err := proto.Unmarshal(raw, cmd); err != nil {
		return Battery{}, err
	}
	b := cmd.GetSystem().GetPower().GetBattery()
	if b == nil || b.Level == nil {
		return Battery{}, errors.New("手环没回电量")
	}
	// state 同 Gadgetbridge：1 充电中
	return Battery{Level: int(b.GetLevel()), Charging: b.GetState() == 1}, nil
}

// Notify 给手环发一条通知（手环会震动；亮不亮屏看手环上的设置）。
func (d *Device) Notify(title, body string) error {
	cmd := &xiaomipb.Command{
		Type:    proto.Uint32(typeNotification),
		Subtype: proto.Uint32(notificationCmdSend),
		Notification: &xiaomipb.Notification{Notification2: &xiaomipb.Notification2{Notification3: &xiaomipb.Notification3{
			Package:   proto.String("dev.herdr.agent"),
			AppName:   proto.String("herdr"),
			Title:     proto.String(title),
			Body:      proto.String(body),
			Unknown4:  proto.String(""),
			Timestamp: proto.String(time.Now().Format("20060102T150405")),
			Id:        proto.Uint32(uint32(time.Now().UnixMilli() & 0x7fffffff)),
		}}},
	}
	return d.SendCommand(cmd)
}
