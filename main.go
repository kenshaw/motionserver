// Command motionserver provides a UDP motion server for use with DualShock 4
// controllers.
//
// udp packet header (ittle endian):
// -----
//   4 byte magic ("DSUC" or "DSUS")
//   2 byte protocol version
//   2 byte message length
//   4 byte ieee crc32 checksum
//   4 byte client/server id
//   4 byte message type
// -----
//  20 total
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kenshaw/evdev"
)

// magic.
const (
	sonyCorp      = 1356
	dualShock4_v1 = 1476
	dualShock4_v2 = 2508
	dualShock3    = 616

	protocolVer = 1001
	maxSlots    = 4
	headerLen   = 20

	msgTypeVersion   = 0x100000
	msgTypeListPorts = 0x100001
	msgTypePadData   = 0x100002

	dsuClient = "DSUC" // DualShockUdpClient
	dsuServer = "DSUS" // DualShockUdpServer
)

// flags.
var (
	flagListen   = flag.String("l", "127.0.0.1:26760", "listen address")
	flagExpiry   = flag.Duration("expiry", 5*time.Second, "client expiry")
	flagServerID = flag.Int("id", 0, "server id")
)

func main() {
	flag.Parse()

	// generate server id if not specified
	if *flagServerID == 0 {
		b := make([]byte, 4)
		_, err := rand.Read(b)
		if err != nil {
			log.Fatal(err)
		}
		*flagServerID = int(ble.Uint32(b))
	}

	// listen
	addr, err := net.ResolveUDPAddr("udp", *flagListen)
	if err != nil {
		log.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	// create context
	ctx, cancel := context.WithCancel(context.Background())

	// run
	go handleExpires(ctx)
	go handlePads(ctx, conn)
	go handleMessages(ctx, conn)

	// wait for sig
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	log.Printf("received signal: %v", <-sigs)
	cancel()
	for _, sl := range slots.vals {
		sl.disconnect(conn)
	}
	time.Sleep(1 * time.Second)
}

// handleExpires handles removing expired client remotes.
func handleExpires(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		time.Sleep(1 * time.Second)

		remotes.Lock()
		n := time.Now()
		for _, r := range remotes.vals {
			r.Lock()
			if n.After(r.all) {
				r.all = time.Time{}
			}
			for i, t := range r.slots {
				if n.After(t) {
					delete(r.slots, i)
				}
			}
			for m, t := range r.macs {
				if n.After(t) {
					delete(r.macs, m)
				}
			}
			r.Unlock()
		}
		remotes.Unlock()
	}
}

// handlePads handles finding gamepads, connecting them to a slot, and starting
// the polling for events.
func handlePads(ctx context.Context, conn *net.UDPConn) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		time.Sleep(1 * time.Second)

		// list available devices
		devices, err := filepath.Glob("/dev/input/event*")
		if err != nil {
			continue
		}

		// iterate devices
		for _, filename := range devices {
			select {
			case <-ctx.Done():
				return
			default:
			}

			// open device
			d := openDev(filename)
			if d == nil {
				continue
			}

			// find slot
			sl := findSlot(d)
			if sl == nil {
				d.Close()
				continue
			}

			// start polling
			log.Printf("[%s] %q (%s) connected to slot %d", d.serial, d.name, d.path, sl.ID)
			ch := make(chan struct{})
			go poll(ctx, conn, d, ch)

			// handle cleanup
			go func() {
				defer d.Close()
				select {
				case <-ctx.Done():
				case <-ch:
				}

				log.Printf("[%s] %q (%s) disconnected from slot %d", d.serial, d.name, d.path, sl.ID)

				pads.Lock()
				defer pads.Unlock()
				delete(pads.vals, d.serial)

				sl.disconnect(conn)
			}()
		}
	}
}

// open opens a evdev device checking if it is an appropriate gamepad.
func openDev(filename string) *dev {
	d, err := evdev.OpenFile(filename)
	if err != nil {
		return nil
	}
	pad := -1
	id := d.ID()
	// check if it is either a dualshock4 or dualShock3 motion device
	if id.Vendor == sonyCorp && strings.Contains(strings.ToLower(d.Name()), "motion") {
		if id.Product == dualShock4_v1 || id.Product == dualShock4_v2 {
			pad = 4
		}
		if id.Product == dualShock3 {
			pad = 3
		}
	}

	if pad == -1 {
		d.Close()
		return nil
	}

	// check all expected axes are present for dualShock4
	if pad == 4 {
		axes := d.AbsoluteTypes()
		for _, a := range []evdev.AbsoluteType{
			evdev.AbsoluteX, evdev.AbsoluteY, evdev.AbsoluteZ,
			evdev.AbsoluteRX, evdev.AbsoluteRY, evdev.AbsoluteRZ,
		} {
			_, ok := axes[a]
			if !ok {
				d.Close()
				return nil
			}
		}
	}
	// check all expected axes are present for dualShock3
	if pad == 3 {
		axes := d.AbsoluteTypes()
		for _, a := range []evdev.AbsoluteType{
			evdev.AbsoluteX, evdev.AbsoluteY, evdev.AbsoluteZ,
		} {
			_, ok := axes[a]
			if !ok {
				d.Close()
				return nil
			}
		}
	}

	pads.Lock()
	defer pads.Unlock()

	// skip if pad already registered
	serial := d.Serial()
	if _, ok := pads.vals[serial]; ok {
		return nil
	}

	// wrap device
	p := &dev{d, d.Name(), serial, filename}
	pads.vals[serial] = p
	return p
}

// findSlot finds appropriate slot for the pad.
func findSlot(d *dev) *Slot {
	slots.Lock()
	defer slots.Unlock()

	var sl *Slot
	var next uint8 = maxSlots
	for i := uint8(0); i < maxSlots; i++ {
		var ok bool
		sl, ok = slots.vals[i]
		if ok && sl.Serial == d.serial {
			break
		} else if next == maxSlots {
			next = i
		}
	}

	// no slots available
	if sl == nil && next >= maxSlots {
		return nil
	}
	mod := ModelDS4
	if d.ID().Product == dualShock3 {
		mod = ModelDS3
	}

	if sl == nil {
		sl = &Slot{
			Serial: d.serial,
			ID:     next,
			Model:  mod,
			MAC:    d.mac(next),
		}
		slots.vals[next] = sl
	}

	sl.State = StateConnected
	sl.ConnectionType = d.connType()
	sl.BatteryStatus = BatteryStatusCharged
	sl.Active = true

	return sl
}

// poll polls input events from the gamepad, sending data messages to
// registered remotes for every sync message.
func poll(ctx context.Context, conn *net.UDPConn, d *dev, closeCh chan struct{}) {
	defer close(closeCh)

	var count uint32
	var report Report
	axes, events := d.AbsoluteTypes(), d.Poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return

		case event := <-events:
			// channel closed
			if event == nil {
				return
			}

			switch typ := event.Type.(type) {
			// send report
			case evdev.SyncType:
				if evdev.SyncType(event.Code) != evdev.SyncReport {
					continue
				}
				count++
				go sendReport(conn, d.serial, count, report)

			// record accelerometer / gyrocope in report
			case evdev.AbsoluteType:
				switch typ {
				// accelerometer
				case evdev.AbsoluteX:
					report.Accl.X = -1 * float32(event.Value) / float32(axes[evdev.AbsoluteX].Res)
				case evdev.AbsoluteY:
					report.Accl.Y = -1 * float32(event.Value) / float32(axes[evdev.AbsoluteY].Res)
				case evdev.AbsoluteZ:
					report.Accl.Z = -1 * float32(event.Value) / float32(axes[evdev.AbsoluteZ].Res)

				// gyroscope
				case evdev.AbsoluteRX:
					report.Gyro.X = float32(event.Value) / float32(axes[evdev.AbsoluteRX].Res)
				case evdev.AbsoluteRY:
					report.Gyro.Y = -1 * float32(event.Value) / float32(axes[evdev.AbsoluteRY].Res)
				case evdev.AbsoluteRZ:
					report.Gyro.Z = -1 * float32(event.Value) / float32(axes[evdev.AbsoluteRZ].Res)
				}

				// motion timestamp expects reporting in microseconds
				report.MotionTimestamp = uint64(event.Time.Nano() / int64(time.Microsecond))
			}
		}
	}
}

// sendReport handles sending outbound message events from gamepads to any
// registered clients.
func sendReport(conn *net.UDPConn, serial string, id uint32, report Report) {
	// find slot
	slots.RLock()
	var sl *Slot
	for i := uint8(0); i < maxSlots; i++ {
		if slots.vals[i].Serial == serial {
			sl = slots.vals[i]
			break
		}
	}
	slots.RUnlock()
	if sl == nil {
		return
	}

	// collect remotes
	var addrs []*net.UDPAddr
	remotes.RLock()
	for _, r := range remotes.vals {
		if !r.all.IsZero() {
			addrs = append(addrs, r.remote)
			continue
		}
		if _, ok := r.slots[sl.ID]; ok {
			addrs = append(addrs, r.remote)
			continue
		}
		if _, ok := r.macs[serial]; ok {
			addrs = append(addrs, r.remote)
			continue
		}
	}
	remotes.RUnlock()

	// no remotes to send to, bail
	if addrs == nil {
		return
	}

	// build message
	msg := buildPadDataMsg(id, sl, &report)
	for _, remote := range addrs {
		go sendMsg(conn, remote, msgTypePadData, msg)
	}
}

// handleMessages handles processing inbound messages.
func handleMessages(ctx context.Context, conn *net.UDPConn) {
	for {
		var buf []byte
		select {
		case <-ctx.Done():
			return
		case buf = <-pool:
		default:
			buf = make([]byte, poolBufLen)
		}
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[unknown] ERROR: could not read from udp: %v", err)
			continue
		}
		go process(conn, remote, buf, n)
	}
}

// process wraps processing a message.
func process(conn *net.UDPConn, remote *net.UDPAddr, buf []byte, n int) {
	if err := processMsg(conn, remote, buf, n); err != nil {
		log.Printf("[%s] ERROR: %v", remote, err)
	}
	select {
	case pool <- buf:
	default:
	}
}

// processMsg processes an inbound message.
func processMsg(conn *net.UDPConn, remote *net.UDPAddr, buf []byte, n int) error {
	// validate type and message
	r, _, msgType, err := validateMsg(buf, n)
	if err != nil {
		return fmt.Errorf("invalid message: %v", err)
	}

	// get message handler
	h, ok := messageHandlers[msgType]
	if !ok {
		return fmt.Errorf("invalid message type %d", msgType)
	}

	// build responses
	if err := h(conn, remote, buf[r:n]); err != nil {
		return fmt.Errorf("invalid message: %v", err)
	}
	return nil
}

// messageHandlers is the map of inbound message types to a handler.
var messageHandlers = map[uint32]func(*net.UDPConn, *net.UDPAddr, []byte) error{
	msgTypeVersion:   versionHandler,
	msgTypeListPorts: listPortsHandler,
	msgTypePadData:   padDataHandler,
}

// versionHandler handles incoming Version responses.
func versionHandler(conn *net.UDPConn, remote *net.UDPAddr, req []byte) error {
	go sendMsg(conn, remote, msgTypeVersion, []byte{})
	return nil
}

// listPortsHandler handles incoming ListPorts messages.
func listPortsHandler(conn *net.UDPConn, remote *net.UDPAddr, req []byte) error {
	// port count
	if len(req) < 4 {
		return errors.New("request too short")
	}
	count := ble.Uint32(req)
	if count < 0 || count > maxSlots {
		return errors.New("invalid count")
	}
	if len(req) != 4+int(count) {
		return errors.New("invalid request length")
	}

	// send response for each requested slot
	for i := 0; i < int(count); i++ {
		s := req[4+i]
		if s < 0 || s >= maxSlots {
			return fmt.Errorf("invalid slot value in offset %d", i)
		}
		slots.RLock()
		sl, ok := slots.vals[s]
		slots.RUnlock()
		if !ok {
			continue
		}
		go sendMsg(conn, remote, msgTypeListPorts, buildSlotMsg(sl))
	}
	return nil
}

// padDataHandler handles incoming PadData messages.
func padDataHandler(conn *net.UDPConn, addr *net.UDPAddr, req []byte) error {
	if len(req) != 8 {
		return errors.New("invalid request length")
	}

	// read flags and slot information
	flags, i, mac := req[0], req[1], fmt.Sprintf("%x", net.HardwareAddr(req[2:]))
	if i < 0 || i >= maxSlots {
		return errors.New("invalid slot")
	}

	remote := addr.String()

	remotes.Lock()
	defer remotes.Unlock()

	// create registration
	if _, ok := remotes.vals[remote]; !ok {
		remotes.vals[remote] = &reg{
			remote: addr,
			slots:  make(map[uint8]time.Time),
			macs:   make(map[string]time.Time),
		}
	}

	remotes.vals[remote].Lock()
	defer remotes.vals[remote].Unlock()

	// update registration
	t := time.Now().Add(*flagExpiry)
	switch {
	case flags == 0:
		remotes.vals[remote].all = t
	case flags&1 != 0:
		remotes.vals[remote].slots[i] = t
	case flags&2 != 0:
		remotes.vals[remote].macs[mac] = t
	}
	return nil
}

// buildSlotMsg builds a connected slot message.
func buildSlotMsg(sl *Slot) []byte {
	//  1 byte id
	//  1 byte state
	//  1 byte model
	//  1 byte connection type
	//  6 byte mac
	//  1 byte battery status
	//  1 byte active
	// ---
	// 12 total
	buf := make([]byte, 12)
	copy(buf, []byte{
		sl.ID,
		sl.State,
		sl.Model,
		sl.ConnectionType,
	})
	copy(buf[4:], sl.MAC)
	buf[10] = sl.BatteryStatus
	buf[11] = boolToByte(sl.Active)
	return buf
}

// buildPadDataMsg builds the PadData status message
//
// 12 device state
//  4 packet counter
//  8 button masks
// 10 button active states
//  2 trigger states
// 12 trackpad states
//  8 motion timestamp
// 12 accelerometer
// 12 gyroscope
// --
// 80 total
func buildPadDataMsg(id uint32, sl *Slot, report *Report) []byte {
	// -- device state (12)
	// 1 byte id
	// 1 byte state
	// 1 byte model
	// 1 byte connection type
	// 6 byte mac
	// 1 byte battery status
	// 1 byte active
	buf := make([]byte, 80)
	copy(buf, []byte{
		sl.ID,
		sl.State,
		sl.Model,
		sl.ConnectionType,
	})
	copy(buf[4:], sl.MAC)
	buf[10] = sl.BatteryStatus
	buf[11] = boolToByte(sl.Active)

	// packet counter 4
	ble.PutUint32(buf[12:], id)

	// -- button masks
	// 1 byte button mask (left,down,right,up,options,R3,L3,share)
	// 1 byte button mask (square,cross,circle,triangle,R1,L1,R2,L2)
	// 1 byte button ps
	// 1 byte button touch
	// 2 byte position left x,y
	// 2 byte position right x,y
	// --
	// 8 total

	// -- button active states
	// 1 byte 0xff dpad left
	// 1 byte 0xff dpad down
	// 1 byte 0xff dpad right
	// 1 byte 0xff dpad up
	// 1 byte 0xff square
	// 1 byte 0xff cross
	// 1 byte 0xff circle
	// 1 byte 0xff triangle
	// 1 byte 0xff R1
	// 1 byte 0xff L2
	// --
	// 10

	// 1 byte 0x00-0xff R2
	// 1 byte 0x00-0xff L2
	// --
	// 2

	// -- trackpad states
	// 1 byte first active 0x01
	// 1 byte first id
	// 2 byte first pad x
	// 2 byte first pad y
	// 1 byte second active 0x01
	// 1 byte second id
	// 2 byte second pad x
	// 2 byte second pad y
	// --
	// 12

	// 8 bytes motion timestamp 48-56
	ble.PutUint64(buf[48:], report.MotionTimestamp)

	// 12 byte accelerometer (x, y, z) 56-68
	putFloat32(buf[56:], report.Accl.X)
	putFloat32(buf[60:], report.Accl.Y)
	putFloat32(buf[64:], report.Accl.Z)

	// 12 byte gyroscope (p, y, r) 68-80
	putFloat32(buf[68:], report.Gyro.X)
	putFloat32(buf[72:], report.Gyro.Y)
	putFloat32(buf[76:], report.Gyro.Z)

	return buf
}

// validateMsg validates an incoming message, returning the header length,
// client ID, and message type.
func validateMsg(buf []byte, n int) (int, uint32, uint32, error) {
	switch {
	case n < headerLen:
		return 0, 0, 0, fmt.Errorf("too short")
	case string(buf[:4]) != dsuClient:
		return 4, 0, 0, fmt.Errorf("invalid magic")
	case ble.Uint16(buf[4:]) != protocolVer:
		return 6, 0, 0, fmt.Errorf("invalid protocol version (%d)", ble.Uint16(buf[4:]))
	}

	// length
	length := ble.Uint16(buf[6:])
	if length < 0 || int(length)+16 != n {
		return 8, 0, 0, fmt.Errorf("invalid length %d", length)
	}

	// checksum
	checksum := ble.Uint32(buf[8:])
	copy(buf[8:], empty)
	if computed := crc32.ChecksumIEEE(buf[:n]); computed != checksum {
		return 8, 0, 0, fmt.Errorf("invalid checksum (expected %d, received %d)", computed, checksum)
	}

	// client id + message type
	clientID := ble.Uint32(buf[12:])
	msgType := ble.Uint32(buf[16:])
	return headerLen, clientID, msgType, nil
}

// sendMsg sends msg to the remote.
func sendMsg(conn *net.UDPConn, remote *net.UDPAddr, msgType uint32, msg []byte) error {
	var buf []byte
	select {
	case buf = <-pool:
	default:
		buf = make([]byte, poolBufLen)
	}

	// build header
	copy(buf, dsuServerMagic)
	ble.PutUint16(buf[4:], protocolVer)
	z := make([]byte, 2)
	ble.PutUint16(z, protocolVer)
	ble.PutUint16(buf[6:], uint16(len(msg)+4))
	copy(buf[8:], empty)
	ble.PutUint32(buf[12:], uint32(*flagServerID))
	ble.PutUint32(buf[16:], msgType)
	copy(buf[headerLen:], msg)

	// checksum
	checksum := crc32.ChecksumIEEE(buf[:headerLen+len(msg)])
	ble.PutUint32(buf[8:], checksum)

	// send
	_, err := conn.WriteToUDP(buf[:headerLen+len(msg)], remote)
	if err != nil {
		log.Printf("[%s] ERROR: could not write udp message: %v", remote, err)
	}

	select {
	case pool <- buf:
	default:
	}

	return nil
}

// putFloat32 is util func to assist with writing float32 to a buffer.
func putFloat32(buf []byte, f float32) {
	ble.PutUint32(buf, math.Float32bits(f))
}

// boolToByte converts a bool to a byte.
func boolToByte(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// dev wraps a evdev.
type dev struct {
	*evdev.Evdev
	name   string
	serial string
	path   string
}

// connType returns the connection type for the device.
func (d *dev) connType() ConnectionType {
	switch d.ID().BusType {
	case evdev.BusUSB:
		return ConnectionTypeUSB
	case evdev.BusBluetooth:
		return ConnectionTypeBluetooth
	}
	return ConnectionTypeNone
}

// mac returns the hardware addr for the device.
func (d *dev) mac(i uint8) net.HardwareAddr {
	if mac, err := net.ParseMAC(d.serial); err == nil {
		return mac
	}
	return net.HardwareAddr{0o0, 0o0, 0o0, 0o0, 0o0, i}
}

// reg holds the time based information for when a
type reg struct {
	remote *net.UDPAddr
	all    time.Time
	slots  map[uint8]time.Time
	macs   map[string]time.Time
	sync.RWMutex
}

const (
	poolBufLen = 128
)

var (
	// pool is a buffer pool.
	pool = make(chan []byte, 20)

	// pads are the connected gamepads.
	pads = struct {
		vals map[string]*dev
		sync.RWMutex
	}{
		vals: make(map[string]*dev),
	}

	// slots maps a connected gamepads.
	slots = struct {
		vals map[uint8]*Slot
		sync.RWMutex
	}{
		vals: make(map[uint8]*Slot, maxSlots),
	}

	// remotes holds the registered clients.
	remotes = struct {
		vals map[string]*reg
		sync.RWMutex
	}{
		vals: make(map[string]*reg),
	}

	// empty is an empty buffer (used to replace checksum values in messages).
	empty = []byte{0, 0, 0, 0}

	// dsuServerMagic is the DSUS server magic as a slice.
	dsuServerMagic = []byte(dsuServer)

	// ble is just a helper for binary.LittleEndian.
	ble = binary.LittleEndian
)
