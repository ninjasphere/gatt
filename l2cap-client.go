// TODO: Figure out about how to structure things for multiple
// OS / BLE interface configurations. Build tags? Subpackages?

package gatt

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/davecgh/go-spew/spew"
)

const (
	ATT_OP_ERROR              = 0x01
	ATT_OP_MTU_REQ            = 0x02
	ATT_OP_MTU_RESP           = 0x03
	ATT_OP_FIND_INFO_REQ      = 0x04
	ATT_OP_FIND_INFO_RESP     = 0x05
	ATT_OP_READ_BY_TYPE_REQ   = 0x08
	ATT_OP_READ_BY_TYPE_RESP  = 0x09
	ATT_OP_READ_REQ           = 0x0a
	ATT_OP_READ_RESP          = 0x0b
	ATT_OP_READ_BLOB_REQ      = 0x0c
	ATT_OP_READ_BLOB_RESP     = 0x0d
	ATT_OP_READ_BY_GROUP_REQ  = 0x10
	ATT_OP_READ_BY_GROUP_RESP = 0x11
	ATT_OP_WRITE_REQ          = 0x12
	ATT_OP_WRITE_RESP         = 0x13
	ATT_OP_HANDLE_NOTIFY      = 0x1b
	ATT_OP_HANDLE_IND         = 0x1d
	ATT_OP_WRITE_CMD          = 0x52

	ATT_ECODE_SUCCESS              = 0x00
	ATT_ECODE_INVALID_HANDLE       = 0x01
	ATT_ECODE_READ_NOT_PERM        = 0x02
	ATT_ECODE_WRITE_NOT_PERM       = 0x03
	ATT_ECODE_INVALID_PDU          = 0x04
	ATT_ECODE_AUTHENTICATION       = 0x05
	ATT_ECODE_REQ_NOT_SUPP         = 0x06
	ATT_ECODE_INVALID_OFFSET       = 0x07
	ATT_ECODE_AUTHORIZATION        = 0x08
	ATT_ECODE_PREP_QUEUE_FULL      = 0x09
	ATT_ECODE_ATTR_NOT_FOUND       = 0x0a
	ATT_ECODE_ATTR_NOT_LONG        = 0x0b
	ATT_ECODE_INSUFF_ENCR_KEY_SIZE = 0x0c
	ATT_ECODE_INVAL_ATTR_VALUE_LEN = 0x0d
	ATT_ECODE_UNLIKELY             = 0x0e
	ATT_ECODE_INSUFF_ENC           = 0x0f
	ATT_ECODE_UNSUPP_GRP_TYPE      = 0x10
	ATT_ECODE_INSUFF_RESOURCES     = 0x11

	GATT_PRIM_SVC_UUID = 0x2800
	GATT_INCLUDE_UUID  = 0x2802
	GATT_CHARAC_UUID   = 0x2803

	GATT_CLIENT_CHARAC_CFG_UUID = 0x2902
	GATT_SERVER_CHARAC_CFG_UUID = 0x2903
)

// newL2cap uses s to provide l2cap access.
func newL2capClient(address string, publicAddress bool) (*l2capClient, error) {

	addressType := "random"
	if publicAddress {
		addressType = "public"
	}

	shim, err := newCShim("l2cap-ble-client", address, addressType)
	if err != nil {
		return nil, err
	}

	c := &l2capClient{
		shim:                   shim,
		readbuf:                bufio.NewReader(shim),
		scanner:                bufio.NewScanner(shim),
		mtu:                    23,
		address:                address,
		publicAddress:          publicAddress,
		security:               "low",
		commands:               make(chan *l2capClientCommand),
		data:                   make(chan []byte),
		connected:              make(chan struct{}),
		quit:                   make(chan struct{}),
		notification:           make(chan *Notification),
		currentCommandComplete: make(chan bool),
	}

	go c.eventloop()
	go c.commandLoop()

	return c, nil
}

type l2capClient struct {
	shim                   shim
	readbuf                *bufio.Reader
	scanner                *bufio.Scanner
	sendmu                 sync.Mutex // serializes writes to the shim
	mtu                    uint16
	address                string
	publicAddress          bool
	serving                bool
	security               string
	currentCommand         *l2capClientCommand
	currentCommandComplete chan bool
	commands               chan *l2capClientCommand
	quit                   chan struct{}
	connected              chan struct{}
	data                   chan []byte
	notification           chan *Notification
	closed                 bool
}

type l2capClientCommand struct {
	buffer        []byte
	callback      func([]byte)
	writeCallback func()
}

type Notification struct {
	Handle uint16
	Data   []byte
}

func (c *l2capClient) upgradeSecurity() error {
	log.Printf("Upgrading security")
	return c.shim.Signal(syscall.SIGUSR2)
}

func (c *l2capClient) close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	//close the c shim to close server
	err := c.shim.Signal(syscall.SIGINT)
	c.shim.Wait()

	c.shim.Close()
	if err != nil {
		println("Failed to send message to l2cap: ", err)
	}
	//call c.quit when close signal of shim arrives

	select {
	case c.quit <- struct{}{}:
		close(c.quit)
	default:
		println("Tried to close an already closed client")
	}

	return nil
}

func (c *l2capClient) eventloop() error {
	for {
		log.Printf("waiting for scanner data")
		c.scanner.Scan()
		err := c.scanner.Err()
		s := c.scanner.Text()

		log.Printf("received: %s\n\n", s)

		if err != nil {
			//return err
		}

		f := strings.Fields(s)
		switch f[0] {
		case "bind":
			log.Printf("Got bind event: %s", f[1])
		case "connect":
			log.Printf("Got connect event: %s", f[1])
			if f[1] == "success" {
				c.connected <- struct{}{}
			}
		case "disconnect":
			log.Printf("Got disconnect event")
			c.close()
			return nil
		case "data":
			data, err := hex.DecodeString(f[1])
			if err != nil {
				log.Fatalf("Failed to parse l2cap-client hex data event: %s", s)
			}
			log.Println("Got data event")

			commandId := data[0]

			switch commandId {
			case ATT_OP_HANDLE_NOTIFY, ATT_OP_HANDLE_IND:
				// log.Print("It's a Notification!")

				var handle uint16
				err := binary.Read(bytes.NewReader(data[1:3]), binary.LittleEndian, &handle)
				if err != nil {
					log.Fatalf("Failed to read notification handle : %s", err)
				}

				c.notification <- &Notification{
					Handle: handle,
					Data:   data[3:],
				}

			default:
				if c.currentCommand != nil {
					log.Print("There is a current command waiting for a response: %s", c.currentCommand)
					command := c.currentCommand
					c.currentCommandComplete <- true
					go command.callback(data)
				} else {
					log.Print("No-one cares about the data i just recieved!")
					spew.Dump(data)
				}
			}

		case "log":
			log.Printf("hci-l2cap-client: %s", s)
		case "security":
			spew.Dump(f)
			level := f[2]
			log.Printf("Got security event: %s", level)
			if level == "low" {
				c.upgradeSecurity()
				time.Sleep(time.Second)
			} else {
				log.Printf("Security upgraded. Resending last command.")
				c.sendCommand(c.currentCommand)
			}
		default:
			log.Fatalf("unexpected event type: %s", s)
		}

	}
	return nil
}

func (c *l2capClient) commandLoop() error {
	for {
		c.currentCommand = nil

		command := <-c.commands
		log.Printf("write	: %X", command.buffer)

		c.sendCommand(command)

		if command.callback != nil {
			// log.Printf("Command has a callback... waiting for data") // XXX timeout?
			c.currentCommand = command
			<-c.currentCommandComplete
		}

	}
	return nil
}

func (c *l2capClient) sendCommand(command *l2capClientCommand) {
	log.Printf("write	: %X", command.buffer)

	c.send(command.buffer)
}

type ServiceDescription struct {
	StartHandle uint16
	EndHandle   uint16
	UUID        string
}

func (c *l2capClient) discoverServices() ([]ServiceDescription, error) {
	log.Printf("Discovering services")

	var services []ServiceDescription

	var currentStartHandle uint16 = 0x0001

	for {
		response, err := c.runCommand(makeReadByGroupRequest(currentStartHandle, 0xffff, GATT_PRIM_SVC_UUID))

		if err != nil {
			return nil, err
		}

		opcode := response[0]

		if opcode == ATT_OP_READ_BY_GROUP_RESP {

			var dataType = int(response[1])

			response = response[2:]

			for i := 0; i < len(response); i += dataType {
				svc := response[i : i+dataType]
				reader := bytes.NewReader(svc)

				service := ServiceDescription{}

				err := binary.Read(reader, binary.LittleEndian, &service.StartHandle)
				if err != nil {
					log.Printf("Failed to read start handle: %s", err)
				}

				err = binary.Read(reader, binary.LittleEndian, &service.EndHandle)
				if err != nil {
					log.Printf("Failed to read end handle: %s", err)
				}

				service.UUID = uuid(svc[4:])

				currentStartHandle = service.EndHandle + 1

				services = append(services, service)
			}

		}

		if opcode != ATT_OP_READ_BY_GROUP_RESP || services[len(services)-1].EndHandle == 0xffff {
			break
		}

	}

	return services, nil
}

type CharacteristicDescription struct {
	StartHandle uint16
	Properties  struct {
		Broadcast                 bool
		Read                      bool
		WriteWithoutResponse      bool
		Write                     bool
		Notify                    bool
		Indicate                  bool
		AuthenticatedSignedWrites bool
		ExtendedProperties        bool
	}
	ValueHandle uint16
	UUID        string
}

func (c *l2capClient) discoverCharacteristics(service ServiceDescription) ([]CharacteristicDescription, error) {
	log.Printf("Discovering characteristics for service: %s", service.UUID)

	var characteristics []CharacteristicDescription

	currentStartHandle := service.StartHandle

	for {
		response, err := c.runCommand(makeReadByTypeRequest(currentStartHandle, service.EndHandle, GATT_CHARAC_UUID))

		if err != nil {
			return nil, err
		}

		opcode := response[0]

		if opcode == ATT_OP_READ_BY_TYPE_RESP {

			var dataType = int(response[1])

			response = response[2:]

			spew.Dump("read response", response, opcode, dataType)

			for i := 0; i < len(response); i += dataType {
				spew.Dump("reading", i, i+dataType, "of length", len(response))
				buf := response[i : i+dataType] // XXX: I seem to get an extra byte on the end?
				spew.Dump("Char", i, buf)
				reader := bytes.NewReader(buf)

				char := CharacteristicDescription{}

				err := binary.Read(reader, binary.LittleEndian, &char.StartHandle)
				if err != nil {
					log.Printf("Failed to read start handle: %s", err)
				}

				var properties uint8

				err = binary.Read(reader, binary.LittleEndian, &properties)
				if err != nil {
					log.Printf("Failed to read properties handle: %s", err)
				}

				err = binary.Read(reader, binary.LittleEndian, &char.ValueHandle)
				if err != nil {
					log.Printf("Failed to read value handle: %s", err)
				}

				char.UUID = uuid(buf[5:])

				char.Properties.Broadcast = properties&0x01 > 0
				char.Properties.Read = properties&0x02 > 0
				char.Properties.WriteWithoutResponse = properties&0x04 > 0
				char.Properties.Write = properties&0x08 > 0
				char.Properties.Notify = properties&0x10 > 0
				char.Properties.Indicate = properties&0x20 > 0
				char.Properties.AuthenticatedSignedWrites = properties&0x40 > 0
				char.Properties.ExtendedProperties = properties&0x80 > 0

				currentStartHandle = char.ValueHandle

				characteristics = append(characteristics, char)
			}

		}

		if opcode != ATT_OP_READ_BY_TYPE_RESP || characteristics[len(characteristics)-1].ValueHandle == service.EndHandle {
			break
		}

	}

	return characteristics, nil
}

/*
L2capBle.prototype.discoverCharacteristics = function(serviceUuid, characteristicUuids) {
	var service = this._services[serviceUuid];
	var characteristics = [];

	this._characteristics[serviceUuid] = {};
	this._descriptors[serviceUuid] = {};

	var callback = function(data) {
		var opcode = data[0];
		var i = 0;

		if (opcode === ATT_OP_READ_BY_TYPE_RESP) {
			var type = data[1];
			var num = (data.length - 2) / type;

			for (i = 0; i < num; i++) {
				characteristics.push({
					startHandle: data.readUInt16LE(2 + i * type + 0),
					properties: data.readUInt8(2 + i * type + 2),
					valueHandle: data.readUInt16LE(2 + i * type + 3),
					uuid: (type == 7) ? data.readUInt16LE(2 + i * type + 5).toString(16) : data.slice(2 + i * type + 5).slice(0, 16).toString('hex').match(/.{1,2}/g).reverse().join('')
				});
			}
		}

		if (opcode !== ATT_OP_READ_BY_TYPE_RESP || characteristics[characteristics.length - 1].valueHandle === service.endHandle) {

			var characteristicsDiscovered = [];
			for (i = 0; i < characteristics.length; i++) {
				var properties = characteristics[i].properties;

				var characteristic = {
					properties: [],
					uuid: characteristics[i].uuid
				};

				if (i !== 0) {
					characteristics[i - 1].endHandle = characteristics[i].startHandle - 1;
				}

				if (i === (characteristics.length - 1)) {
					characteristics[i].endHandle = service.endHandle;
				}

				this._characteristics[serviceUuid][characteristics[i].uuid] = characteristics[i];

				if (properties & 0x01) {
					characteristic.properties.push('broadcast');
				}

				if (properties & 0x02) {
					characteristic.properties.push('read');
				}

				if (properties & 0x04) {
					characteristic.properties.push('writeWithoutResponse');
				}

				if (properties & 0x08) {
					characteristic.properties.push('write');
				}

				if (properties & 0x10) {
					characteristic.properties.push('notify');
				}

				if (properties & 0x20) {
					characteristic.properties.push('indicate');
				}

				if (properties & 0x40) {
					characteristic.properties.push('authenticatedSignedWrites');
				}

				if (properties & 0x80) {
					characteristic.properties.push('extendedProperties');
				}

				if (characteristicUuids.length === 0 || characteristicUuids.indexOf(characteristic.uuid) !== -1) {
					characteristicsDiscovered.push(characteristic);
				}
			}

			this.emit('characteristicsDiscover', this._address, serviceUuid, characteristicsDiscovered);
		} else {
			this._queueCommand(this.readByTypeRequest(characteristics[characteristics.length - 1].valueHandle + 1, service.endHandle, GATT_CHARAC_UUID), callback);
		}
	}.bind(this);

	this._queueCommand(this.readByTypeRequest(service.startHandle, service.endHandle, GATT_CHARAC_UUID), callback);
};*/

func uuid(s []byte) string {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
	return hex.EncodeToString(s)
}

func (c *l2capClient) runCommand(command []byte) ([]byte, error) {
	done := make(chan []byte, 1)

	c.queueCommand(&l2capClientCommand{
		buffer: command,
		callback: func(response []byte) {
			done <- response
		},
	})

	select {
	case <-time.After(time.Second * 5):
		return nil, fmt.Errorf("Timed out after 5 seconds")
	case response := <-done:
		return response, nil
	}
}

func (c *l2capClient) queueCommand(command *l2capClientCommand) {
	//log.Print("Queuing command % X", command.buffer)
	c.commands <- command
}

func (c *l2capClient) notify(enable bool, startHandle uint16, endHandle uint16, useNotify bool, useIndicate bool) {
	// log.Printf("Calling notify: %t %d %d %t %t", enable, startHandle, endHandle, useNotify, useIndicate)
	c.queueCommand(&l2capClientCommand{
		buffer: makeReadByTypeRequest(startHandle, endHandle, GATT_CLIENT_CHARAC_CFG_UUID),
		callback: func(data []byte) {
			// log.Printf("Got notify response %x", data)

			type Response struct {
				Opcode, DataType uint8
				Handle, Value    uint16
			}

			//XXX: Fix this dumb code.
			var response Response
			err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &response)
			if err != nil {
				log.Fatalf("Failed to read first notification response : %s", err)
			}

			if response.Opcode != ATT_OP_READ_BY_TYPE_RESP {
				log.Printf("Got the wrong response type in notify!! %x", response.Opcode) //XXX:
				return
			}

			if enable {
				if useNotify {
					response.Value |= 0x0001
				} else if useIndicate {
					response.Value |= 0x0002
				}
			} else {
				if useNotify {
					response.Value &= 0xfffe
				} else if useIndicate {
					response.Value &= 0xfffd
				}
			}

			buf := new(bytes.Buffer)
			err = binary.Write(buf, binary.LittleEndian, response.Value)
			if err != nil {
				log.Fatalf("Notify binary.Write failed: %s", err)
			}

			c.queueCommand(&l2capClientCommand{
				buffer: makeWriteRequest(response.Handle, buf.Bytes(), false),
				callback: func(data []byte) {
					// log.Printf("Got notify final response %s", data)
					// spew.Dump(data)
				},
			})
		},
	})
}

func (c *l2capClient) send(b []byte) error {
	if len(b) > int(c.mtu) {
		panic(fmt.Errorf("cannot send %x: mtu %d", b, c.mtu))
	}

	// log.Printf("L2CAP-client: Sending % X", b)
	c.sendmu.Lock()
	_, err := fmt.Fprintf(c.shim, "%x\n", b)
	c.sendmu.Unlock()
	return err
}

func (c *l2capClient) writeByHandle(handle uint16, data []byte) {
	c.queueCommand(&l2capClientCommand{
		buffer: makeWriteRequest(handle, data, false),
		callback: func(data []byte) {
			log.Printf("Got writebyhandle final response %s", data)
			spew.Dump(data)
		},
	})
}

func (c *l2capClient) readByHandle(handle uint16) chan []byte {
	response := make(chan []byte, 1)
	// log.Printf("L2CAP-client: Queuing readByHandle %d", handle)
	c.queueCommand(&l2capClientCommand{
		buffer: makeReadRequest(handle),
		callback: func(data []byte) {
			response <- data
		},
	})
	return response
}

// func (c *l2capClient) readByHandle(handle uint16, uuid uint16) chan []byte {
// 	response := make(chan []byte, 1)
// 	log.Printf("L2CAP-client: Queuing readByHandle %d", handle)
// 	c.queueCommand(&l2capClientCommand{
// 		buffer: makeReadByGroupRequest(handle, handle+1, uuid),
// 		callback: func(data []byte) {
// 			response <- data
// 		},
// 	})
// 	return response
// }

/* Command Definitions XXX: Clean this up */

const readByGroupRequest uint8 = 0x10

type readByGroupRequestPacket struct {
	startHandle uint16
	endHandle   uint16
	groupUuid   uint16
}

func makeReadByGroupRequest(startHandle uint16, endHandle uint16, groupUuid uint16) []byte {
	return encodeCommand(readByGroupRequest, &readByGroupRequestPacket{
		startHandle: startHandle,
		endHandle:   endHandle,
		groupUuid:   groupUuid,
	})
}

const readByTypeRequest uint8 = 0x08
const readByTypeResponse uint8 = 0x09

type readByTypeRequestPacket struct {
	startHandle uint16
	endHandle   uint16
	groupUuid   uint16
}

func makeReadByTypeRequest(startHandle uint16, endHandle uint16, groupUuid uint16) []byte {
	return encodeCommand(readByTypeRequest, &readByTypeRequestPacket{
		startHandle: startHandle,
		endHandle:   endHandle,
		groupUuid:   groupUuid,
	})
}

const readRequest uint8 = 0x0a

type readRequestPacket struct {
	handle uint16
}

func makeReadRequest(handle uint16) []byte {
	// log.Printf("--makeReadRequest-- %d", handle)
	return encodeCommand(readRequest, &readRequestPacket{
		handle: handle,
	})
}

const writeRequest = 0x12
const writeCommand = 0x52

type x struct {
	h uint16
}

func makeWriteRequest(handle uint16, data []byte, withoutResponse bool) []byte {
	var command uint8
	if withoutResponse {
		command = writeCommand
	} else {
		command = writeRequest
	}
	return append(encodeCommand(command, x{handle}), data...)
}

func encodeCommand(id uint8, command interface{}) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, id)

	err := binary.Write(buf, binary.LittleEndian, command)
	if err != nil {
		fmt.Printf("binary.Write failed encoding command:0x%x - %s", id, err)
	}

	return buf.Bytes()
}

func (c *l2capClient) disconnect() error {
	return c.shim.Signal(syscall.SIGHUP)
}

func (c *l2capClient) SendRawCommands(strcmds []string) {
	bytecmds := make([][]byte, len(strcmds))
	for i := range bytecmds {
		bytecmds[i] = make([]byte, len(strcmds[i]))
		bytes, err := hex.DecodeString(strcmds[i])
		if err != nil {
			log.Fatalf("Problem encoding to bytes ", strcmds[i])
		}
		bytecmds[i] = bytes
	}

	for _, cmd := range bytecmds {
		c.queueCommand(&l2capClientCommand{
			buffer: cmd,
			callback: func(response []byte) {
				// log.Printf("received %s after sending %s raw command", hex.EncodeToString(response), hex.EncodeToString(cmd))
			},
		})
	}
}
