package gatt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"

	"github.com/potix/gatt/linux"
)

type peripheral struct {
	// NameChanged is called whenever the peripheral GAP device name has changed.
	NameChanged func(*peripheral)

	// ServicedModified is called when one or more service of a peripheral have changed.
	// A list of invalid service is provided in the parameter.
	ServicesModified func(*peripheral, []*Service)

	d    *device
	svcs []*Service

	sub *subscriber

	mtu uint16
	l2c io.ReadWriteCloser

	lastop byte
	outreqc chan message
	inresc  chan []byte
	quitc   chan struct{}

	pd *linux.PlatData // platform specific data
}

func (p *peripheral) Device() Device       { return p.d }
func (p *peripheral) ID() string           { return strings.ToUpper(net.HardwareAddr(p.pd.Address[:]).String()) }
func (p *peripheral) MTU() int             { return int(p.mtu) }
func (p *peripheral) Name() string         { return p.pd.Name }
func (p *peripheral) Services() []*Service { return p.svcs }
func (p *peripheral) Close() error	   { return nil }

func finish(op byte, h uint16, b []byte) bool {
	done := b[0] == attOpError && b[1] == op && b[2] == byte(h) && b[3] == byte(h>>8)
	e := attEcode(b[4])
	if e != attEcodeAttrNotFound {
		// log.Printf("unexpected protocol error: %s", e)
		// FIXME: terminate the connection
	}
	return done
}

func (p *peripheral) DiscoverServices(ds []UUID) ([]*Service, error) {
	// p.pd.Conn.Write([]byte{0x02, 0x87, 0x00}) // MTU
	done := false
	start := uint16(0x0001)
	for !done {
		op := byte(attOpReadByGroupReq)
		b := make([]byte, 7)
		b[0] = op
		binary.LittleEndian.PutUint16(b[1:3], start)
		binary.LittleEndian.PutUint16(b[3:5], 0xFFFF)
		binary.LittleEndian.PutUint16(b[5:7], 0x2800)

		b = p.sendReq(op, b)
		if finish(op, start, b) {
			break
		}
		b = b[1:]
		l, b := int(b[0]), b[1:]
		switch {
		case l == 6 && (len(b)%6 == 0):
		case l == 20 && (len(b)%20 == 0):
		default:
			return nil, ErrInvalidLength
		}

		for len(b) != 0 {
			endh := binary.LittleEndian.Uint16(b[2:4])
			u := UUID{b[4:l]}

			if UUIDContains(ds, u) {
				s := &Service{
					uuid: u,
					h:    binary.LittleEndian.Uint16(b[:2]),
					endh: endh,
				}
				p.svcs = append(p.svcs, s)
			}

			b = b[l:]
			done = endh == 0xFFFF
			start = endh + 1
		}
	}
	return p.svcs, nil
}

func (p *peripheral) DiscoverIncludedServices(ss []UUID, s *Service) ([]*Service, error) {
	// TODO
	return nil, nil
}

func (p *peripheral) DiscoverCharacteristics(cs []UUID, s *Service) ([]*Characteristic, error) {
	done := false
	start := s.h
	var prev *Characteristic
	for !done {
		op := byte(attOpReadByTypeReq)
		b := make([]byte, 7)
		b[0] = op
		binary.LittleEndian.PutUint16(b[1:3], start)
		binary.LittleEndian.PutUint16(b[3:5], s.endh)
		binary.LittleEndian.PutUint16(b[5:7], 0x2803)

		b = p.sendReq(op, b)
		if finish(op, start, b) {
			break
		}
		b = b[1:]

		l, b := int(b[0]), b[1:]
		switch {
		case l == 7 && (len(b)%7 == 0):
		case l == 21 && (len(b)%21 == 0):
		default:
			return nil, ErrInvalidLength
		}

		for len(b) != 0 {
			h := binary.LittleEndian.Uint16(b[:2])
			props := Property(b[2])
			vh := binary.LittleEndian.Uint16(b[3:5])
			u := UUID{b[5:l]}
			s := searchService(p.svcs, h, vh)
			if s == nil {
				log.Printf("Can't find service range that contains 0x%04X - 0x%04X", h, vh)
				return nil, fmt.Errorf("Can't find service range that contains 0x%04X - 0x%04X", h, vh)
			}
			c := &Characteristic{
				uuid:  u,
				svc:   s,
				props: props,
				h:     h,
				vh:    vh,
			}
			if UUIDContains(cs, u) {
				s.chars = append(s.chars, c)
			}
			b = b[l:]
			done = vh == s.endh
			start = vh + 1
			if prev != nil {
				prev.endh = c.h - 1
			}
			prev = c
		}
	}
	if len(s.chars) > 1 {
		s.chars[len(s.chars)-1].endh = s.endh
	}
	return s.chars, nil
}

func (p *peripheral) DiscoverDescriptors(ds []UUID, c *Characteristic) ([]*Descriptor, error) {
	done := false
	start := c.vh + 1
	for !done {
		if c.endh == 0 {
			c.endh = c.svc.endh
		}
		op := byte(attOpFindInfoReq)
		b := make([]byte, 5)
		b[0] = op
		binary.LittleEndian.PutUint16(b[1:3], start)
		binary.LittleEndian.PutUint16(b[3:5], c.endh)

		b = p.sendReq(op, b)
		if finish(attOpFindInfoReq, start, b) {
			break
		}
		b = b[1:]

		var l int
		f, b := int(b[0]), b[1:]
		switch {
		case f == 1 && (len(b)%4 == 0):
			l = 4
		case f == 2 && (len(b)%18 == 0):
			l = 18
		default:
			return nil, ErrInvalidLength
		}

		for len(b) != 0 {
			h := binary.LittleEndian.Uint16(b[:2])
			u := UUID{b[2:l]}
			d := &Descriptor{uuid: u, h: h, char: c}
			if UUIDContains(ds, u) {
				c.descs = append(c.descs, d)
			}
			if u.Equal(attrClientCharacteristicConfigUUID) {
				c.cccd = d
			}
			b = b[l:]
			done = h == c.endh
			start = h + 1
		}
	}
	return c.descs, nil
}

func (p *peripheral) ReadCharacteristic(c *Characteristic) ([]byte, error) {
	b := make([]byte, 3)
	op := byte(attOpReadReq)
	b[0] = op
	binary.LittleEndian.PutUint16(b[1:3], c.vh)

	b = p.sendReq(op, b)
	b = b[1:]
	return b, nil
}

func (p *peripheral) ReadLongCharacteristic(c *Characteristic) ([]byte, error) {
	// The spec says that a read blob request should fail if the characteristic
	// is smaller than mtu - 1.  To simplify the API, the first read is done
	// with a regular read request.  If the buffer received is equal to mtu -1,
	// then we read the rest of the data using read blob.
	firstRead, err := p.ReadCharacteristic(c)
	if err != nil {
		return nil, err
	}
	if len(firstRead) < int(p.mtu)-1 {
		return firstRead, nil
	}

	var buf bytes.Buffer
	buf.Write(firstRead)
	off := uint16(len(firstRead))
	for {
		b := make([]byte, 5)
		op := byte(attOpReadBlobReq)
		b[0] = op
		binary.LittleEndian.PutUint16(b[1:3], c.vh)
		binary.LittleEndian.PutUint16(b[3:5], off)

		b = p.sendReq(op, b)
		b = b[1:]
		if len(b) == 0 {
			break
		}
		buf.Write(b)
		off += uint16(len(b))
		if len(b) < int(p.mtu)-1 {
			break
		}
	}
	return buf.Bytes(), nil
}

func (p *peripheral) WriteCharacteristic(c *Characteristic, value []byte, noRsp bool) error {
	b := make([]byte, 3+len(value))
	op := byte(attOpWriteReq)
	b[0] = op
	if noRsp {
		b[0] = attOpWriteCmd
	}
	binary.LittleEndian.PutUint16(b[1:3], c.vh)
	copy(b[3:], value)

	if noRsp {
		p.sendCmd(op, b)
		return nil
	}
	b = p.sendReq(op, b)
	// TODO: error handling
	b = b[1:]
	return nil
}

func (p *peripheral) ReadDescriptor(d *Descriptor) ([]byte, error) {
	b := make([]byte, 3)
	op := byte(attOpReadReq)
	b[0] = op
	binary.LittleEndian.PutUint16(b[1:3], d.h)

	b = p.sendReq(op, b)
	b = b[1:]
	// TODO: error handling
	return b, nil
}

func (p *peripheral) WriteDescriptor(d *Descriptor, value []byte) error {
	b := make([]byte, 3+len(value))
	op := byte(attOpWriteReq)
	b[0] = op
	binary.LittleEndian.PutUint16(b[1:3], d.h)
	copy(b[3:], value)

	b = p.sendReq(op, b)
	b = b[1:]
	// TODO: error handling
	return nil
}

func (p *peripheral) setNotifyValue(c *Characteristic, flag uint16,
	f func(*Characteristic, []byte, error)) error {
	if c.cccd == nil {
		return errors.New("no cccd") // FIXME
	}
	ccc := uint16(0)
	if f != nil {
		ccc = flag
		p.sub.subscribe(c.vh, func(b []byte, err error) { f(c, b, err) })
	}
	b := make([]byte, 5)
	op := byte(attOpWriteReq)
	b[0] = op
	binary.LittleEndian.PutUint16(b[1:3], c.cccd.h)
	binary.LittleEndian.PutUint16(b[3:5], ccc)

	b = p.sendReq(op, b)
	b = b[1:]
	// TODO: error handling
	if f == nil {
		p.sub.unsubscribe(c.vh)
	}
	return nil
}

func (p *peripheral) SetNotifyValue(c *Characteristic,
	f func(*Characteristic, []byte, error)) error {
	return p.setNotifyValue(c, gattCCCNotifyFlag, f)
}

func (p *peripheral) SetIndicateValue(c *Characteristic,
	f func(*Characteristic, []byte, error)) error {
	return p.setNotifyValue(c, gattCCCIndicateFlag, f)
}

func (p *peripheral) ReadRSSI() int {
	// TODO: implement
	return -1
}

func searchService(ss []*Service, start, end uint16) *Service {
	for _, s := range ss {
		if s.h < start && s.endh >= end {
			return s
		}
	}
	return nil
}

// TODO: unifiy the message with OS X pots and refactor
type message struct {
	op   byte
	b    []byte
}

func (p *peripheral) sendCmd(op byte, b []byte) {
	fmt.Printf("sendCmd op = %v, data= %v\n", op, b)
	p.outreqc <- message{op: op, b: b}
}

func (p *peripheral) sendReq(op byte, b []byte) []byte {
	m := message{op: op, b: b}
	p.outreqc <- m
	return <- p.inresc
}

func (p *peripheral) loop() {
	// Serialize the request.
	inreqc := make(chan []byte)
	inresc := make(chan []byte)

	// Dequeue request loop
	go func() {
		for {
			select {
			case poutreq := <- p.outreqc:
				p.lastop = poutreq.op
				p.l2c.Write(poutreq.b)
                        case inres := <- inresc:
				if (attRspFor[p.lastop] == inres[0] || 
				    attOpError == inres[0]) {
					p.inresc <- inres
				} else {
					log.Printf("unexpected operation code 0x%02x", inres[0])
				}
			case inreq := <- inreqc:
				var resp []byte
				switch reqType, req := inreq[0], inreq[1:]; reqType {
				case attOpMtuReq:
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp) // TODO
				case attOpFindInfoReq:
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp) // TODO
				case attOpFindByTypeValueReq:
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp) // TODO
				case attOpReadByTypeReq:
					resp = p.handleReadByType(req)
				case attOpReadReq:
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp) // TODO
				case attOpReadBlobReq:
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp) // TODO
				case attOpReadByGroupReq:
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp) // TODO
				case attOpWriteReq:
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp) // TODO
				case attOpWriteCmd:
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp) // TODO
				case attOpReadMultiReq:
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp) // TODO
				case attOpPrepWriteReq:
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp) // TODO
				case attOpExecWriteReq:
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp) // TODO
				case attOpSignedWriteCmd:
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp) // TODO
				default:
					log.Printf("unexpected operation code 0x%02x", reqType)
					resp = attErrorRsp(reqType, 0x0000, attEcodeReqNotSupp)
				}
				if resp != nil {
					p.l2c.Write(resp)
				}
			case <-p.quitc:
				return
			}
		}
	}()

	// L2CAP implementations shall support a minimum MTU size of 48 bytes.
	// The default value is 672 bytes
	buf := make([]byte, 672)

	// Handling response or notification/indication
	for {
		n, err := p.l2c.Read(buf)
		if n == 0 || err != nil {
			close(p.quitc)
			return
		}

		b := make([]byte, n)
		copy(b, buf)

		_, isinres := attReqFor[b[0]]
		_, isinreq := attRspFor[b[0]]
		if (isinreq || b[0] == attOpWriteCmd || b[0] == attOpSignedWriteCmd) {
			// request or command from periferal
			inreqc <- b 
		} else if (b[0] == attOpHandleNotify || b[0] == attOpHandleInd) {
			// indication or notification from periferal
			h := binary.LittleEndian.Uint16(b[1:3])
			f := p.sub.fn(h)
			if f == nil { 
				log.Printf("notified by unsubscribed handle")
				// FIXME: terminate the connection?
			} else {
				go f(b[3:], nil)
			}
			if b[0] == attOpHandleInd {
				// write aknowledgement for indication
				p.l2c.Write([]byte{attOpHandleCnf})
			}
		} else if (isinres || b[0] == attOpError) {
			// error or response from periferal
			inresc <- b
		} else {
			// unexpected operation type
			log.Printf("unexpected operation code 0x%02x", b[0])
		}
	}
}

func (p *peripheral) SetMTU(mtu uint16) error {
	b := make([]byte, 3)
	op := byte(attOpMtuReq)
	b[0] = op
	binary.LittleEndian.PutUint16(b[1:3], uint16(mtu))

	b = p.sendReq(op, b)
	serverMTU := binary.LittleEndian.Uint16(b[1:3])
	if serverMTU < mtu {
		mtu = serverMTU
	}
	p.mtu = mtu
	return nil
}

// REQ: ReadByType(0x08), StartHandle, EndHandle, Type(UUID)
// RSP: ReadByType(0x09), LenOfEachDataField, DataField, DataField, ...
func (p *peripheral) handleReadByType(b []byte) []byte {
        start, end := readHandleRange(b[:4])
        t := UUID{b[4:]}

        w := newL2capWriter(p.mtu)
        w.WriteByteFit(attOpReadByTypeRsp)
        uuidLen := -1
        for _, a := range p.d.attrs.Subrange(start, end) {
                if !a.typ.Equal(t) {
                        continue
                }
                if (a.secure&CharRead) != 0 {
                        return attErrorRsp(attOpReadByTypeReq, start, attEcodeAuthentication)
                }
                v := a.value
                if v == nil {
                        rsp := newResponseWriter(int(p.mtu - 1))
                        req := &ReadRequest{
                                Request: Request{Central: p},
                                Cap:     int(p.mtu - 1),
                                Offset:  0,
                        }
                        if c, ok := a.pvt.(*Characteristic); ok {
                                c.rhandler.ServeRead(rsp, req)
                        } else if d, ok := a.pvt.(*Descriptor); ok {
                                d.rhandler.ServeRead(rsp, req)
                        }
                        v = rsp.bytes()
                }
                if uuidLen == -1 {
                        uuidLen = len(v)
                        w.WriteByteFit(byte(uuidLen) + 2)
                }
                if len(v) != uuidLen {
                        break
                }
                w.Chunk()
                w.WriteUint16Fit(a.h)
                w.WriteFit(v)
                if ok := w.Commit(); !ok {
			log.Printf("L2capWriter faied in commit")
                        break
                }
        }
        if uuidLen == -1 {
                return attErrorRsp(attOpReadByTypeReq, start, attEcodeAttrNotFound)
        }
	return w.Bytes()
}
