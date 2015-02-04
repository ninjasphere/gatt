package gatt

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	//"bytes"
	"github.com/davecgh/go-spew/spew"
	//"encoding/binary"
	//"os"
)

// A Client is a GATT client. Client are single-shot types; once
// a Client has been closed, it cannot be restarted. Instead, create
// a new Client. Only one client may be running at a time.
// XXX: This description came from the Server class, might not make sense...
type Client struct {

	// HCI is the hci device to use, e.g. "hci1".
	// If HCI is "", an hci device will be selected
	// automatically.
	HCI string

	// Closed is an optional callback function that will be called
	// when the Client is closed. err will be any associated error.
	// If the server was closed by calling Close, err may be nil.
	Closed func(error)

	// StateChange is an optional callback function that will be called
	// when the client changes states.
	StateChange func(newState string)

	Discover func(device *DiscoveredDevice)

	Advertisement func(device *DiscoveredDevice)

	Rssi func(address string, name string, rssi int8)

	devices map[string]*DiscoveredDevice

	hci *hciClient

	quitonce sync.Once
	quit     chan struct{}
	err      error
	//
	// hciShim shim
}

type DiscoveredDevice struct {
	Address        string
	PublicAddress  bool
	Rssi           int8
	Advertisement  DeviceAdvertisement
	discoveryCount int
	l2cap          *l2capClient
	Notification   func(notification *Notification)
	Disconnected   func()
	Services       map[string]ServiceDescription
}

type DeviceAdvertisement struct {
	LocalName        string
	TxPowerLevel     int8
	ManufacturerData []byte
	ServiceData      []ServiceData
	ServiceUuids     map[string]bool
	Flags            DeviceAdvertisementFlags
	ClassOfDevice    []byte
}

type ServiceData struct {
	Uuid string
	Data []byte
}

// Bluetooth Core Specification Supplement (CSS) 0.4 - 1.3.1 Flags Description
type DeviceAdvertisementFlags struct {
	LimitedDiscoverableMode                              bool
	GeneralDiscoverableMode                              bool
	BREDRNotSupported                                    bool
	SimulataneousLEAndBREDRToSameDeviceCapableController bool
	SimulataneousLEAndBREDRToSameDeviceCapableHost       bool
}

func (s *Client) Start() error {

	hciShim, err := newCShim("hci-ble-client", s.HCI)
	if err != nil {
		return err
	}

	// s.hciShim = hciShim
	s.hci = newHCIClient(hciShim)
	event, data, err := s.hci.event()
	if err != nil {
		return err
	}
	if event != "adapterState" {
		return fmt.Errorf("unexpected hci event: %q", event)
	}
	if data == "unauthorized" {
		return errors.New("unauthorized; does l2cap-ble have the correct permissions?")
	}
	if data != "poweredOn" {
		return fmt.Errorf("unexpected adapter state: %q", data)
	}
	// TODO: If you kill and restart the server quickly, you get event
	// "unsupported". Waiting and then starting again fixes it.
	// Figure out why, and handle it automatically.

	if s.StateChange != nil {
		s.StateChange(data)
	}

	go func() {
		for {
			// No need to check s.quit here; if the users closes the server,
			// hci will get killed, which'll cause an error to be returned here.
			event, data, err := s.hci.event()
			//log.Printf("event: %s data: %s err:%s", event, data, err)
			if err != nil {
				break
			}

			if event == "adapterState" && s.StateChange != nil {
				s.StateChange(data)
			} else if event == "event" {
				s.handleAdvertisingEvent(data)
			}
		}
		s.close(err)
	}()

	if s.Closed != nil {
		go func() {
			<-s.quit
			s.Closed(s.err)
		}()
	}

	/*	l2capShim, err := newCShim("l2cap-ble", hciDevice)
		if err != nil {
			s.close(err)
			return err
		}

		s.l2cap = newL2cap(l2capShim)*/
	return nil
}

func (c *Client) StartScanning(allowDuplicates bool) error {
	return c.hci.startScanning(allowDuplicates)
}

// Close stops the Client.
func (s *Client) Close() error {
	err := s.hci.Close()
	s.hci.Wait()
	/*l2caperr := s.l2cap.close()
	if err == nil {
		err = l2caperr
	}*/
	s.close(err)
	// s.hciShim.Close()
	//serverRunningMu.Lock()
	//serverRunning = false
	//serverRunningMu.Unlock()
	return err
}

func (c *Client) handleAdvertisingEvent(data string) error {

	fields := strings.Split(data, ",")

	// log.Printf("Advertising event! : %q", fields)
	// spew.Dump(fields)

	address := fields[0]
	publicAddress := fields[1] == "public"
	eir, err := hex.DecodeString(fields[2])
	if err != nil {
		return fmt.Errorf("Failed to parse eir hex data: %s", fields[2])
	}
	rssi, err := strconv.ParseInt(fields[3], 10, 8)
	if err != nil {
		return fmt.Errorf("Failed to parse rssi: %s", fields[3])
	}

	if c.devices == nil {
		c.devices = make(map[string]*DiscoveredDevice)
	}

	device := c.devices[address]

	if device == nil {
		// log.Printf("Discovered a new device: %s", address)
		device = &DiscoveredDevice{
			Address:       address,
			PublicAddress: publicAddress,
			Advertisement: DeviceAdvertisement{
				ServiceUuids: make(map[string]bool),
				ServiceData:  make([]ServiceData, 0),
			},
		}
		c.devices[address] = device
	}

	device.Rssi = int8(rssi)

	advertisement := &device.Advertisement

	i := 0

	for (i + 1) < len(eir) {

		length := int(eir[i])
		dataType := int(eir[i+1]) // https://www.bluetooth.org/en-us/specification/assigned-numbers/generic-access-profile

		if (i + length + 1) > len(eir) {
			log.Printf("Invalid EIR data, out of range of buffer length")
			break
		}

		var payload []uint8

		if length > 0 {
			payload = eir[i+2 : i+2+length-1]
		}

		switch dataType {
		case 0x01: // Flags
			/*
				Bluetooth Core Specification:
				Vol. 3, Part C, section 8.1.3 (v2.1 + EDR, 3.0 + HS and 4.0)
				Vol. 3, Part C, sections 11.1.3 and 18.1 (v4.0)
				Core Specification Supplement, Part A, section 1.3
			*/
			flags := DeviceAdvertisementFlags{}

			if isFlagSet(0, payload[0]) {
				flags.LimitedDiscoverableMode = true
			}
			if isFlagSet(1, payload[0]) {
				flags.GeneralDiscoverableMode = true
			}
			if isFlagSet(2, payload[0]) {
				flags.BREDRNotSupported = true
			}
			if isFlagSet(3, payload[0]) {
				flags.SimulataneousLEAndBREDRToSameDeviceCapableController = true
			}
			if isFlagSet(4, payload[0]) {
				flags.SimulataneousLEAndBREDRToSameDeviceCapableHost = true
			}

			advertisement.Flags = flags

		case 0x02, // Incomplete List of 16-bit Service Class UUID
			0x03, // Complete List of 16-bit Service Class UUIDs
			0x04, // Complete List of 32-bit Service Class UUIDs
			0x05, // Complete List of 32-bit Service Class UUIDs
			0x06, // Incomplete List of 128-bit Service Class UUIDs
			0x07: // Complete List of 128-bit Service Class UUIDs

			var uuidLength int // 16-bit
			if dataType > 0x05 {
				uuidLength = 16 // 128-bit
			} else if dataType > 0x03 {
				uuidLength = 4 // 32-bit
			} else {
				uuidLength = 2 // 32-bit
			}

			for j := 0; j < len(payload); j += uuidLength {
				uuid := payload[j : j+uuidLength]

				for i, j := 0, len(uuid)-1; i < j; i, j = i+1, j-1 {
					uuid[i], uuid[j] = uuid[j], uuid[i]
				}

				serviceUuid := hex.EncodeToString(uuid)
				advertisement.ServiceUuids[serviceUuid] = true
			}

		case 0x08, // Shortened Local Name
			0x09: // Complete Local Name
			advertisement.LocalName = string(payload)

		case 0x0a: // Tx Power Level
			advertisement.TxPowerLevel = int8(payload[0])

		case 0x0d: // ClassOfDevice
			advertisement.ClassOfDevice = payload

		case 0x12: // Slave Connection Interval Range

		case 0x16: // Service Data, there can be multiple occurences
			if len(payload) > 2 {
				advertisement.ServiceData = append(advertisement.ServiceData, ServiceData{
					Uuid: hex.EncodeToString(payload[0:2]),
					Data: payload[3:],
				})
			}

		case 0x19: // Appearance

		case 0xff: // Manufacturer Specific Data
			advertisement.ManufacturerData = payload

		default:
			log.Printf("Unhandled eir data type: 0x%x", dataType)
			log.Printf("Payload dump:")
			spew.Dump(payload)
		}

		i += (length + 1)
	}

	if c.Rssi != nil {
		c.Rssi(address, device.Advertisement.LocalName, int8(rssi))
	}

	device.discoveryCount += 1

	if c.Advertisement != nil && device.discoveryCount%2 == 0 {
		c.Advertisement(device)
	}

	if c.Discover != nil && device.discoveryCount == 2 {
		c.Discover(device)
	}

	return nil
}

func isFlagSet(pos uint8, b byte) bool {
	return uint8(b)>>pos&1 > 0
}

func (c *Client) close(err error) {
	c.quitonce.Do(func() {
		c.err = err
		close(c.quit)
	})
}

func (d *DiscoveredDevice) Connect() error {

	if d.l2cap != nil {
		return fmt.Errorf("Device %s is already connected", d.Address)
	}

	l2cap, err := newL2capClient(d.Address, d.PublicAddress)
	if err != nil {
		return err
	}

	d.l2cap = l2cap

	select {
	case <-l2cap.connected:
		go func() {
			for {
				notification := <-l2cap.notification
				if d.Notification != nil {
					go d.Notification(notification)
				}
			}
		}()

		go func() {
			<-l2cap.quit
			d.l2cap = nil
			if d.Disconnected != nil {
				go d.Disconnected()
			}
		}()

		go func() {
			for {
				if d.l2cap == nil {
					break
				}
				notification := <-d.l2cap.notification
				if d.Notification != nil {
					go d.Notification(notification)
				}
			}
		}()
		return nil
	case <-time.After(time.Second * 5):
		d.Disconnect()
		return fmt.Errorf("Connection timed out after 5 seconds")
	}

}

func (d *DiscoveredDevice) Disconnect() error {
	return d.l2cap.close()
}

func (d *DiscoveredDevice) DiscoverServices() error {
	services, err := d.l2cap.discoverServices()

	if err == nil {
		d.Services = make(map[string]ServiceDescription)

		for _, service := range services {
			d.Services[service.UUID] = service
		}
	}

	return err
}

func (d *DiscoveredDevice) DiscoverCharacteristics(service ServiceDescription) (map[string]CharacteristicDescription, error) {
	chars, err := d.l2cap.discoverCharacteristics(service)

	if err != nil {
		return nil, err
	}

	characteristics := make(map[string]CharacteristicDescription)

	for _, char := range chars {
		characteristics[char.UUID] = char
	}

	return characteristics, nil
}

// XXX: HACKHACK: This is only temporary till the api is fleshed out
func (d *DiscoveredDevice) Notify(enable bool, startHandle uint16, endHandle uint16, useNotify bool, useIndicate bool) {
	d.l2cap.notify(enable, startHandle, endHandle, useNotify, useIndicate)
}

func (d *DiscoveredDevice) WriteByHandle(handle uint16, data []byte) {
	d.l2cap.writeByHandle(handle, data)
}

func (d *DiscoveredDevice) Write(char CharacteristicDescription, data []byte) {
	spew.Dump("writing char", char, data)
	d.l2cap.writeByHandle(char.ValueHandle, data)
}

func (d *DiscoveredDevice) Read(char CharacteristicDescription) chan []byte {
	return d.l2cap.readByHandle(char.StartHandle)
}

func (d *DiscoveredDevice) ReadByHandle(handle uint16) chan []byte {
	return d.l2cap.readByHandle(handle)
}

func (d *DiscoveredDevice) SendRawCommands(strcmds []string) {
	d.l2cap.SendRawCommands(strcmds)
}

func (d *DiscoveredDevice) UpgradeSecurity() error {
	return d.l2cap.upgradeSecurity()
}
