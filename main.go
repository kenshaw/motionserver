// Command motionserver provides a UDP motion server for use with DualShock 4
// controllers.
//
// Packet notes:
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
//
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
	sonyCorp   = 1356
	dualShock4 = 2508

	protocolVer = 1001
	maxPads     = 4
	headerLen   = 20
	poolBufLen  = 128

	msgTypeVersion   = 0x100000
	msgTypeListPorts = 0x100001
	msgTypePadData   = 0x100002

	dsuClient = "DSUC" // DualShockUdpClient
	dsuServer = "DSUS" // DualShockUdpServer
)

var (
	flagListen   = flag.String("l", "127.0.0.1:26760", "listen address")
	flagExpiry   = flag.Duration("expiry", 1*time.Minute, "client expiry")
	flagServerID = flag.Int("id", 0, "server id")
	flagDevPath  = flag.String("path", "/dev/input/event21", "device path")
)

func main() {
	flag.Parse()

	// genearte server id if not specified
	if *flagServerID == 0 {
		b := make([]byte, 4)
		_, err := rand.Read(b)
		if err != nil {
			log.Fatal(err)
		}
		*flagServerID = int(ble.Uint32(b))
	}

	// udp listen
	addr, err := net.ResolveUDPAddr("udp", *flagListen)
	if err != nil {
		log.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	ctxt, cancel := context.WithCancel(context.Background())
	go findpads(ctxt)
	go recv(ctxt, conn)
	go send(ctxt, conn)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	s := <-sigs
	log.Printf("received signal: %v", s)
	cancel()
}

// findpads handles searching for connected devices.
func findpads(ctxt context.Context) {
	for {
		select {
		case <-ctxt.Done():
			return
		default:
		}
		time.Sleep(1 * time.Second)
		devices, err := filepath.Glob("/dev/input/event*")
		if err != nil {
			continue
		}
		for _, n := range devices {
			d, err := evdev.OpenFile(n)
			if err != nil {
				continue
			}

			// not dualshock4 motion device
			if id := d.ID(); id.Vendor != sonyCorp || id.Product != dualShock4 ||
				!strings.Contains(strings.ToLower(d.Name()), "motion") {
				d.Close()
				continue
			}

			// check all expected axes are present
			axes := d.AbsoluteTypes()
			for _, a := range []evdev.AbsoluteType{
				evdev.AbsoluteX, evdev.AbsoluteY, evdev.AbsoluteZ,
				evdev.AbsoluteRX, evdev.AbsoluteRY, evdev.AbsoluteRZ,
			} {
				_, ok := axes[a]
				if !ok {
					d.Close()
					continue
				}
			}

			// add and start polling
			pollpad(ctxt, n, d)
		}
	}
}

// pollpad finds an empty slot, and starts polling the pad for events.
func pollpad(ctxt context.Context, n string, d *evdev.Evdev) {
	slots.Lock()
	defer slots.Unlock()

	var i int
	dserial := d.Serial()
	for ; i < maxPads; i++ {
		if _, ok := slots.vals[i]; !ok {
			break
		}
		// already registered
		if dserial == slots.vals[i].serial {
			d.Close()
			return
		}
	}
	if i >= maxPads {
		d.Close()
		return
	}

	log.Printf("[%s] connecting %q (%s)", dserial, d.Name(), n)
	connType := ConnectionTypeNone
	switch d.ID().BusType {
	case evdev.BusUSB:
		connType = ConnectionTypeUSB
	case evdev.BusBluetooth:
		connType = ConnectionTypeBluetooth
	}
	serial, err := net.ParseMAC(dserial)
	if err != nil {
		serial = net.HardwareAddr{00, 00, 00, 00, 00, uint8(i)}
	}

	ctxt, cancel := context.WithCancel(ctxt)
	sl := &slot{
		d:      d,
		path:   n,
		serial: dserial,
		pad: Pad{
			ID:             uint8(i),
			State:          StateConnected,
			ConnectionType: connType,
			Model:          ModelDS4,
			MAC:            serial,
			BatteryStatus:  BatteryStatusCharged,
			Active:         true,
		},
		cancel: cancel,
	}
	slots.vals[i] = sl
	ch, err := d.Poll(ctxt, 64)
	if err != nil {
		return
	}
	go poll(ctxt, sl, ch)
}

// poll polls for input events from the gamepad.
func poll(ctxt context.Context, sl *slot, ch <-chan evdev.Event) {
	axes := sl.d.AbsoluteTypes()
	axes = axes
	for {
		select {
		case <-ctxt.Done():
			return

		case event := <-ch:
			if event.Type != evdev.EventAbsolute {
				continue
			}
			/*scale your floats
			  to Gs for acclerometer and deg/sec for gyro*/
			sl.Lock()
			if sl.report == nil {
				sl.report = new(Report)
			}

			switch evdev.AbsoluteType(event.Code) {
			// accelerometer
			case evdev.AbsoluteX:
				sl.report.Accl.X = -1 * float32(event.Value) / float32(axes[evdev.AbsoluteX].Res)
			case evdev.AbsoluteY:
				sl.report.Accl.Y = -1 * float32(event.Value) / float32(axes[evdev.AbsoluteY].Res)
			case evdev.AbsoluteZ:
				sl.report.Accl.Z = -1 * float32(event.Value) / float32(axes[evdev.AbsoluteZ].Res)

			// gyroscope
			case evdev.AbsoluteRX:
				sl.report.Gyro.X = float32(event.Value) / float32(axes[evdev.AbsoluteRX].Res)
			case evdev.AbsoluteRY:
				sl.report.Gyro.Y = -1 * float32(event.Value) / float32(axes[evdev.AbsoluteRY].Res)
			case evdev.AbsoluteRZ:
				sl.report.Gyro.Z = -1 * float32(event.Value) / float32(axes[evdev.AbsoluteRZ].Res)
			}

			sl.report.MotionTimestamp = uint64(event.Time.Nano() / int64(time.Microsecond))
			//sl.report.MotionTimestamp = uint64(time.Now().UnixNano() / int64(time.Microsecond))

			sl.Unlock()
		}
	}
}

// send handles sending outbound message events from any gamepads to registered clients.
func send(ctxt context.Context, conn *net.UDPConn) {
	var i int
	for {
		select {
		case <-ctxt.Done():
			return
		default:
		}
		remotes.RLock()
		for _, reg := range remotes.vals {
			reg.RLock()
			for slot, _ := range reg.slots {
				msg := buildPadDataMsg(i, slot)
				if msg == nil {
					continue
				}
				go sendMsg(conn, reg.remote, reg.clientID, msgTypePadData, msg)
				i++
			}
			reg.RUnlock()
		}
		remotes.RUnlock()
		time.Sleep(1 * time.Millisecond)
	}
}

// recv handles processing inbound messages.
func recv(ctxt context.Context, conn *net.UDPConn) {
	for {
		var buf []byte
		select {
		case <-ctxt.Done():
			return
		case buf = <-pool:
		default:
			buf = make([]byte, poolBufLen)
		}
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[unknown] (unknown) ERROR: could not read from udp: %v", err)
			continue
		}
		go process(conn, remote, buf, n)
	}
}

// process wraps processing a message.
func process(conn *net.UDPConn, remote *net.UDPAddr, buf []byte, n int) {
	//log.Printf(">>> read: %x", buf[:n])
	clientID, err := processMsg(conn, remote, buf, n)
	if err != nil {
		log.Printf("[%s] %d ERROR: %v", remote, clientID)
	}
	select {
	case pool <- buf:
	default:
	}
}

// processMsg processes an inbound message.
func processMsg(conn *net.UDPConn, remote *net.UDPAddr, buf []byte, n int) (uint32, error) {
	// validate type and message
	r, clientID, msgType, err := validateMsg(buf, n)
	if err != nil {
		return clientID, fmt.Errorf("invalid message: %v", err)
	}
	m, ok := messageTypes[msgType]
	if !ok {
		return clientID, fmt.Errorf("invalid message type %d", msgType)
	}

	//log.Printf("[%s] %d received %s request", remote, clientID, m.name)

	// build responses
	res, err := m.f(remote, clientID, buf[r:n])
	if err != nil {
		return clientID, fmt.Errorf("invalid %s message: %v", m.name, err)
	}

	// send responses
	for _, msg := range res {
		if msg == nil {
			continue
		}
		//log.Printf("[%s] %d sending %s response", remote, clientID, m.name)
		go sendMsg(conn, remote, clientID, msgType, msg)
	}

	return clientID, nil
}

// validateMsg reads and validates an incoming UDP message.
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
func sendMsg(conn *net.UDPConn, remote *net.UDPAddr, clientID, msgType uint32, msg []byte) error {
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

	//log.Printf("buf: %x", buf[:headerLen+len(msg)])

	// checksum
	checksum := crc32.ChecksumIEEE(buf[:headerLen+len(msg)])
	ble.PutUint32(buf[8:], checksum)

	//log.Printf("sending: %x", buf[:headerLen+len(msg)])
	_, err := conn.WriteToUDP(buf[:headerLen+len(msg)], remote)
	if err != nil {
		log.Printf("[%s] %d ERROR: could not write udp message: %v", remote, clientID, err)
	}

	select {
	case pool <- buf:
	default:
	}

	return nil
}

// messageTypes is the map of inbound message types and their response
// handlers.
var messageTypes = map[uint32]struct {
	name string
	f    func(*net.UDPAddr, uint32, []byte) ([][]byte, error)
}{
	msgTypeVersion:   {"Version", versionHandler},
	msgTypeListPorts: {"ListPorts", listPortsHandler},
	msgTypePadData:   {"PadData", padDataHandler},
}

var verRes = [][]byte{[]byte{}}

// versionHandler creates Version responses.
func versionHandler(*net.UDPAddr, uint32, []byte) ([][]byte, error) {
	return verRes, nil
}

// listPortsHandler creates the ListPorts response.
func listPortsHandler(remote *net.UDPAddr, clientID uint32, req []byte) ([][]byte, error) {
	// port count
	if len(req) < 4 {
		return nil, errors.New("request too short")
	}
	count := ble.Uint32(req)
	if count < 0 || count > maxPads {
		return nil, errors.New("invalid count")
	}
	if len(req) != 4+int(count) {
		return nil, errors.New("invalid request length")
	}

	// build response
	res := make([][]byte, count)
	for i := 0; i < int(count); i++ {
		// grab associated pad
		s := int(req[4+i])
		if s < 0 || s >= maxPads {
			return nil, fmt.Errorf("invalid slot value in offset %d", i)
		}
		slots.RLock()
		sl, ok := slots.vals[s]
		slots.RUnlock()
		if !ok {
			// this should probably be a fatal error...
			continue
		}

		// response format
		// 1 byte id
		// 1 byte state
		// 1 byte model
		// 1 byte connection type
		// 6 byte mac
		// 1 byte battery status
		// 1 byte active
		buf := make([]byte, 12)
		copy(buf, []byte{
			sl.pad.ID,
			sl.pad.State,
			sl.pad.Model,
			sl.pad.ConnectionType,
		})
		copy(buf[4:], sl.pad.MAC)
		buf[10] = sl.pad.BatteryStatus
		buf[11] = 1 // byte(pad.Active)
		res[i] = buf
	}
	return res, nil
}

// padDataHandler creates the PadData response.
func padDataHandler(addr *net.UDPAddr, clientID uint32, req []byte) ([][]byte, error) {
	if len(req) != 8 {
		return nil, errors.New("invalid request length")
	}

	flags, slot, mac := req[0], int(req[1]), fmt.Sprintf("%x", net.HardwareAddr(req[2:]))
	if slot < 0 || slot >= maxPads {
		return nil, errors.New("invalid slot")
	}

	remote := addr.String()

	remotes.RLock()
	unregistered := remotes.vals[remote] == nil
	remotes.RUnlock()

	// create registration
	if unregistered {
		remotes.Lock()
		remotes.vals[remote] = &reg{
			remote:   addr,
			clientID: clientID,
			slots:    make(map[int]time.Time),
			macs:     make(map[string]time.Time),
		}
		remotes.Unlock()
	}

	remotes.vals[remote].Lock()
	defer remotes.vals[remote].Unlock()

	now := time.Now()
	switch {
	case flags == 0:
		remotes.vals[remote].all = now
	case flags&1 != 0:
		remotes.vals[remote].slots[slot] = now
	case flags&2 != 0:
		remotes.vals[remote].macs[mac] = now
	}
	return nil, nil
}

// buildPadDataMsg builds the PadData status message
func buildPadDataMsg(i, s int) []byte {
	slots.RLock()
	sl, ok := slots.vals[s]
	slots.RUnlock()
	if !ok {
		return nil
	}

	sl.RLock()
	defer sl.RUnlock()
	if sl.report == nil {
		return nil
	}

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
		sl.pad.ID,
		sl.pad.State,
		sl.pad.Model,
		sl.pad.ConnectionType,
	})
	copy(buf[4:], sl.pad.MAC)
	buf[10] = sl.pad.BatteryStatus
	buf[11] = 1 //byte(pad.Active)

	// packet counter 4
	ble.PutUint32(buf[12:], uint32(i))

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

	// 8 bytes motion timestamp (low, high) 48-56
	ble.PutUint64(buf[48:], sl.report.MotionTimestamp)

	// 12 byte accelerometer (x, y, z) 56-68
	putFloat32(buf[56:], sl.report.Accl.X)
	putFloat32(buf[60:], sl.report.Accl.Y)
	putFloat32(buf[64:], sl.report.Accl.Z)

	// 12 byte gyroscope (p, y, r) 68-80
	putFloat32(buf[68:], sl.report.Gyro.X)
	putFloat32(buf[72:], sl.report.Gyro.Y)
	putFloat32(buf[76:], sl.report.Gyro.Z)

	// -----
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

	return buf
}

// slot holds information on a slot.
type slot struct {
	d      *evdev.Evdev
	path   string
	serial string
	pad    Pad
	cancel context.CancelFunc
	report *Report
	sync.RWMutex
}

// reg holds the time based information for when a
type reg struct {
	remote   *net.UDPAddr
	clientID uint32
	all      time.Time
	slots    map[int]time.Time
	macs     map[string]time.Time
	sync.RWMutex
}

// putFloat32 is util func to assist with writing float32 to a buffer.
func putFloat32(buf []byte, f float32) {
	ble.PutUint32(buf, math.Float32bits(f))
}

var (
	// pool is a buffer pool.
	pool = make(chan []byte, 20)

	// slots holds connected gamepads.
	slots = struct {
		vals map[int]*slot
		sync.RWMutex
	}{
		vals: make(map[int]*slot, maxPads),
	}

	// remotes holds the registered clients
	remotes = struct {
		vals map[string]*reg
		sync.RWMutex
	}{
		vals: make(map[string]*reg),
	}

	// empty is an empty buffer (used to replace checksum values in messages).
	empty          = []byte{0, 0, 0, 0}
	dsuServerMagic = []byte(dsuServer)

	ble = binary.LittleEndian
)
