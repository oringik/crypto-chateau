package multiplex_conn

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type MultiplexConnPool struct {
	tcpConn          net.Conn
	currentRequestID atomic.Uint32

	toWriteQueue  chan ToWriteMsg
	listenClients chan *MultiplexConn

	multiplexConnByRequestID   map[uint32]*MultiplexConn
	multiplexConnByRequestIDMx sync.RWMutex

	closeConnsCh chan uint32
	terminateCh  chan bool

	isClient bool
}

func NewMultiplexConnPool(tcpConn net.Conn, isClient bool) *MultiplexConnPool {
	return &MultiplexConnPool{
		tcpConn:          tcpConn,
		currentRequestID: atomic.Uint32{},

		toWriteQueue: make(chan ToWriteMsg),

		listenClients: make(chan *MultiplexConn),

		multiplexConnByRequestID:   make(map[uint32]*MultiplexConn),
		multiplexConnByRequestIDMx: sync.RWMutex{},

		closeConnsCh: make(chan uint32),
		terminateCh:  make(chan bool),

		isClient: isClient,
	}
}

func (p *MultiplexConnPool) NewMultiplexConn() *MultiplexConn {
	requestID := p.currentRequestID.Add(1)

	newMultiplexConn := &MultiplexConn{
		requestID:          requestID,
		localAddr:          p.tcpConn.LocalAddr(),
		remoteAddr:         p.tcpConn.RemoteAddr(),
		readQueue:          make(chan []byte, 5),
		writeQueue:         p.toWriteQueue,
		errChan:            make(chan error),
		connReservedDataMx: sync.Mutex{},
		connReservedData:   []byte{},
		closeCh:            p.closeConnsCh,
		readDeadline:       5 * time.Minute,
	}

	p.multiplexConnByRequestIDMx.Lock()
	p.multiplexConnByRequestID[requestID] = newMultiplexConn
	p.multiplexConnByRequestIDMx.Unlock()

	if !p.isClient {
		p.listenClients <- newMultiplexConn
	}

	return newMultiplexConn
}

func (p *MultiplexConnPool) SetRawTCPReadDeadline(t time.Time) error {
	return p.tcpConn.SetReadDeadline(t)
}

func (p *MultiplexConnPool) Close() {
	p.terminateCh <- true
}

func (p *MultiplexConnPool) ListenClients() chan *MultiplexConn {
	return p.listenClients
}

func (p *MultiplexConnPool) Run() {
	go func() {
		for {
			select {
			case toWriteMsg := <-p.toWriteQueue:
				dataWithRequestID := make([]byte, 0, len(toWriteMsg.Data)+2)
				dataWithRequestID = append(dataWithRequestID, byte(uint16(toWriteMsg.RequestID)), byte(uint16(toWriteMsg.RequestID)>>8))
				dataWithRequestID = append(dataWithRequestID, toWriteMsg.Data...)

				_, err := p.tcpConn.Write(dataWithRequestID)
				var multiplexConn *MultiplexConn
				p.multiplexConnByRequestIDMx.RLock()
				multiplexConn = p.multiplexConnByRequestID[toWriteMsg.RequestID]
				p.multiplexConnByRequestIDMx.RUnlock()

				multiplexConn.errChan <- err
			case requestID := <-p.closeConnsCh:
				p.multiplexConnByRequestIDMx.Lock()
				delete(p.multiplexConnByRequestID, requestID)
				p.multiplexConnByRequestIDMx.Unlock()
			case <-p.terminateCh:
				p.tcpConn.Close()
				close(p.toWriteQueue)
				close(p.listenClients)
				close(p.closeConnsCh)
				return
			}
		}
	}()

	go func() {
		for {
			buf := make([]byte, 4096)
			n, err := p.tcpConn.Read(buf)
			if err != nil {
				p.tcpConn.Close()
				return
			}

			if n == 0 {
				p.tcpConn.Close()
				break
			}

			buf = buf[:n]

			requestID := uint16(buf[0]) | uint16(buf[1])<<8
			p.multiplexConnByRequestIDMx.Lock()
			if multiplexConn, ok := p.multiplexConnByRequestID[uint32(requestID)]; ok {
				multiplexConn.readQueue <- buf[2:]
			} else {
				newMultiplexConn := &MultiplexConn{
					requestID:          p.currentRequestID.Add(1),
					localAddr:          p.tcpConn.LocalAddr(),
					remoteAddr:         p.tcpConn.RemoteAddr(),
					readQueue:          make(chan []byte, 5),
					writeQueue:         p.toWriteQueue,
					errChan:            make(chan error),
					connReservedDataMx: sync.Mutex{},
					connReservedData:   []byte{},
					closeCh:            p.closeConnsCh,
					readDeadline:       5 * time.Minute,
				}

				newMultiplexConn.readQueue <- buf[2:]
				p.multiplexConnByRequestID[uint32(requestID)] = newMultiplexConn

				p.listenClients <- newMultiplexConn
			}
			p.multiplexConnByRequestIDMx.Unlock()
		}
	}()
}

type MultiplexConn struct {
	localAddr  net.Addr
	remoteAddr net.Addr

	requestID uint32

	readQueue  chan []byte
	writeQueue chan<- ToWriteMsg

	errChan chan error

	connReservedDataMx sync.Mutex
	connReservedData   []byte

	readDeadline time.Duration

	closeCh chan<- uint32
}

func (cn *MultiplexConn) Write(p []byte) (int, error) {
	cn.writeQueue <- ToWriteMsg{
		RequestID: cn.requestID,
		Data:      p,
	}
	err := <-cn.errChan

	return len(p), err
}

func (cn *MultiplexConn) Read(b []byte) (int, error) {
	cn.connReservedDataMx.Lock()
	if len(cn.connReservedData) > 0 {
		defer cn.connReservedDataMx.Unlock()

		if len(cn.connReservedData) > len(b) {
			copy(b, cn.connReservedData)
			cn.connReservedData = cn.connReservedData[len(b):]
			return len(b), nil
		}

		copy(b, cn.connReservedData)
		return len(cn.connReservedData), nil
	}
	cn.connReservedDataMx.Unlock()

	select {
	case data := <-cn.readQueue:
		returnLength := len(data)

		cn.connReservedDataMx.Lock()
		if len(data) > len(b) {
			cn.connReservedData = data[len(b):]
			returnLength = len(b)
		}
		cn.connReservedDataMx.Unlock()

		copy(b, data)

		return returnLength, nil
	case <-time.After(cn.readDeadline):
		return 0, io.EOF
	}
}

func (cn *MultiplexConn) Close() error {
	cn.closeCh <- cn.requestID
	return nil
}

func (cn *MultiplexConn) LocalAddr() net.Addr {
	return cn.localAddr
}

func (cn *MultiplexConn) RemoteAddr() net.Addr {
	return cn.remoteAddr
}

func (cn *MultiplexConn) SetDeadline(t time.Time) error {
	return nil
}

func (cn *MultiplexConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (cn *MultiplexConn) SetWriteDeadline(t time.Time) error {
	return nil
}
