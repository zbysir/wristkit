package xiaomi

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	"miband/internal/pb/xiaomipb"
)

// 表盘：列出、切换、删除、安装。协议同 Gadgetbridge（XiaomiWatchfaceService）：
//   - 命令 type 4，subtype 0 列表、1 设为当前、2 删除、4 准备安装
//   - 安装：4/4 带表盘 id 和大小 → 手环回 installStatus（0 可以装）→ 大文件通道传文件（类型 16）→ 4/1 设为当前

const (
	typeWatchface = 4

	watchfaceCmdList    = 0
	watchfaceCmdSet     = 1
	watchfaceCmdDelete  = 2
	watchfaceCmdInstall = 4

	massDataTypeWatchface = 16
)

// Watchface 是手环上装的一个表盘。
type Watchface struct {
	ID        string
	Name      string
	Active    bool
	CanDelete bool
}

func (d *Device) watchfaceRequest(subtype uint32, wf *xiaomipb.Watchface, timeout time.Duration) (*xiaomipb.Watchface, error) {
	data, err := proto.Marshal(&xiaomipb.Command{Type: proto.Uint32(typeWatchface), Subtype: proto.Uint32(subtype), Watchface: wf})
	if err != nil {
		return nil, err
	}
	raw, err := d.Request(typeWatchface, subtype, data, subtype, timeout)
	if err != nil {
		return nil, err
	}
	cmd := &xiaomipb.Command{}
	if err := proto.Unmarshal(raw, cmd); err != nil {
		return nil, err
	}
	return cmd.GetWatchface(), nil
}

// Watchfaces 列出手环上的表盘。
func (d *Device) Watchfaces() ([]Watchface, error) {
	wf, err := d.watchfaceRequest(watchfaceCmdList, nil, 10*time.Second)
	if err != nil {
		return nil, err
	}
	var list []Watchface
	for _, it := range wf.GetWatchfaceList().GetWatchface() {
		list = append(list, Watchface{ID: it.GetId(), Name: it.GetName(), Active: it.GetActive(), CanDelete: it.GetCanDelete()})
	}
	return list, nil
}

// SetWatchface 把表盘设为当前表盘。
func (d *Device) SetWatchface(id string) error {
	return d.watchfaceCommand(watchfaceCmdSet, id)
}

// DeleteWatchface 删掉表盘。
func (d *Device) DeleteWatchface(id string) error {
	return d.watchfaceCommand(watchfaceCmdDelete, id)
}

// watchfaceCommand 发设为当前、删除这类只带 id 的命令；手环回 ack=1 算成功。
func (d *Device) watchfaceCommand(subtype uint32, id string) error {
	wf, err := d.watchfaceRequest(subtype, &xiaomipb.Watchface{WatchfaceId: proto.String(id)}, 10*time.Second)
	if err != nil {
		return err
	}
	if wf.Ack != nil && wf.GetAck() != 1 {
		return fmt.Errorf("手环没接受（ack=%d）", wf.GetAck())
	}
	return nil
}

// WatchfaceID 读表盘文件头里的 id（偏移 0x28 起的 ASCII 数字）。
func WatchfaceID(data []byte) (string, error) {
	if len(data) < 0x40 || !bytes.Equal(data[:4], []byte{0x5A, 0xA5, 0x34, 0x12}) {
		return "", errors.New("不是表盘文件")
	}
	id := data[0x28:0x40]
	if i := bytes.IndexByte(id, 0); i >= 0 {
		id = id[:i]
	}
	if len(id) == 0 {
		return "", errors.New("表盘文件里没有 id")
	}
	return string(id), nil
}

// InstallWatchface 装表盘并设为当前；progress(i, n) 在每个分片被确认后调用。
func (d *Device) InstallWatchface(data []byte, progress func(i, n int)) (string, error) {
	id, err := WatchfaceID(data)
	if err != nil {
		return "", err
	}
	slog.Info("安装表盘", "id", id, "bytes", len(data))
	wf, err := d.watchfaceRequest(watchfaceCmdInstall, &xiaomipb.Watchface{WatchfaceInstallStart: &xiaomipb.WatchfaceInstallStart{
		Id: proto.String(id), Size: proto.Uint32(uint32(len(data))),
	}}, 10*time.Second)
	if err != nil {
		return "", err
	}
	// 0 可以装；2 手环上已经有同 id 的表盘，也照样传（覆盖）
	if st := wf.GetInstallStatus(); st != 0 && st != 2 {
		return "", fmt.Errorf("手环拒绝安装表盘（installStatus=%d）", st)
	}
	if err := d.massSend(massDataTypeWatchface, data, progress); err != nil {
		return "", err
	}
	// 传完手环要解析一会儿才能切过去
	var setErr error
	for range 5 {
		time.Sleep(2 * time.Second)
		if setErr = d.SetWatchface(id); setErr == nil {
			break
		}
	}
	return id, setErr
}
