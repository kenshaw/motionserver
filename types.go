package main

import (
	"net"
)

type State = uint8

const (
	StateDisconnected State = 0x00
	StateReserved     State = 0x01
	StateConnected    State = 0x02
)

type ConnectionType = uint8

const (
	ConnectionTypeNone      ConnectionType = 0x00
	ConnectionTypeUSB       ConnectionType = 0x01
	ConnectionTypeBluetooth ConnectionType = 0x02
)

type Model = uint8

const (
	ModelNone    Model = 0
	ModelDS3     Model = 1
	ModelDS4     Model = 2
	ModelGeneric Model = 3
)

type BatteryStatus = uint8

const (
	BatteryStatusNone     BatteryStatus = 0x00
	BatteryStatusDying    BatteryStatus = 0x01
	BatteryStatusLow      BatteryStatus = 0x02
	BatteryStatusMedium   BatteryStatus = 0x03
	BatteryStatusHigh     BatteryStatus = 0x04
	BatteryStatusFull     BatteryStatus = 0x05
	BatteryStatusCharging BatteryStatus = 0xEE
	BatteryStatusCharged  BatteryStatus = 0xEF
)

type Slot struct {
	Serial         string
	ID             uint8
	State          State
	ConnectionType ConnectionType
	Model          Model
	MAC            net.HardwareAddr
	BatteryStatus  BatteryStatus
	Active         bool
}

// disconnect clears the device state and sends one last report.
func (sl *Slot) disconnect(conn *net.UDPConn) {
	sl.State = StateDisconnected
	sl.ConnectionType = ConnectionTypeNone
	sl.BatteryStatus = BatteryStatusNone
	sl.Active = false
	go sendReport(conn, sl.Serial, 0, Report{})
}

type Btn int

const (
	BtnShare Btn = 0x01
	BtnL3    Btn = 0x02
)

type Report struct {
	PacketCounter   uint64
	MotionTimestamp uint64
	Button          struct {
		R1       bool
		L1       bool
		R2       bool
		L2       bool
		R3       bool
		L3       bool
		PS       bool
		Square   bool
		Cross    bool
		Circle   bool
		Triangle bool
		Options  bool
		Share    bool
		DPad     struct {
			Up    bool
			Right bool
			Left  bool
			Down  bool
		}
		Touch bool
	}
	Position struct {
		Left struct {
			X float32
			Y float32
		}
		Right struct {
			X float32
			Y float32
		}
	}
	Trigger struct {
		L2 float32
		R2 float32
	}
	Accl struct {
		X float32
		Y float32
		Z float32
	}
	Gyro struct {
		X float32
		Y float32
		Z float32
	}
	TrackPad struct {
		First struct {
			Active bool
			ID     uint8
			X      uint16
			Y      uint16
		}
		Second struct {
			Active bool
			ID     uint8
			X      uint16
			Y      uint16
		}
	}
}
