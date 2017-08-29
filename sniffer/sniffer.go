package sniffer

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/negbie/heplify/config"
	"github.com/negbie/heplify/logp"
	"github.com/tsg/gopacket"
	"github.com/tsg/gopacket/layers"
	"github.com/tsg/gopacket/pcap"
)

type SnifferSetup struct {
	pcapHandle     *pcap.Handle
	afpacketHandle *afpacketHandle
	config         *config.InterfacesConfig
	isAlive        bool
	dumper         *pcap.Dumper

	// bpf filter
	filter string

	// Decoder    *decoder.DecoderStruct
	worker     Worker
	DataSource gopacket.PacketDataSource
}

type Worker interface {
	OnPacket(data []byte, ci *gopacket.CaptureInfo)
}

type WorkerFactory func(layers.LinkType) (Worker, error)

func (sniffer *SnifferSetup) setFromConfig(cfg *config.InterfacesConfig) error {
	var err error
	sniffer.config = cfg

	if index, err := strconv.Atoi(sniffer.config.Device); err == nil { // Device is numeric
		devices, err := ListDeviceNames(false, false)
		if err != nil {
			return fmt.Errorf("Error getting devices list: %v", err)
		}
		sniffer.config.Device, err = deviceNameFromIndex(index, devices)
		if err != nil {
			return fmt.Errorf("Couldn't understand device index %d: %v", index, err)
		}
		logp.Info("Resolved device index %d to device: %s", index, sniffer.config.Device)
	}

	if sniffer.config.Snaplen == 0 {
		sniffer.config.Snaplen = 65535
	}

	if sniffer.config.Type == "autodetect" || sniffer.config.Type == "" {
		sniffer.config.Type = "pcap"
	}

	logp.Debug("sniffer", "Sniffer type: %s device: %s", sniffer.config.Type, sniffer.config.Device)

	switch sniffer.config.Type {
	case "pcap":
		if len(sniffer.config.File) > 0 {
			sniffer.pcapHandle, err = pcap.OpenOffline(sniffer.config.File)
			if err != nil {
				return err
			}
		} else {
			sniffer.pcapHandle, err = pcap.OpenLive(
				sniffer.config.Device,
				int32(sniffer.config.Snaplen),
				true,
				500*time.Millisecond)
			if err != nil {
				return err
			}
			err = sniffer.pcapHandle.SetBPFFilter(sniffer.filter)
			if err != nil {
				return err
			}
		}

		sniffer.DataSource = gopacket.PacketDataSource(sniffer.pcapHandle)

	case "af_packet":
		if sniffer.config.BufferSizeMb == 0 {
			sniffer.config.BufferSizeMb = 32
		}

		szFrame, szBlock, numBlocks, err := afpacketComputeSize(
			sniffer.config.BufferSizeMb,
			sniffer.config.Snaplen,
			os.Getpagesize())
		if err != nil {
			logp.Err("sniffer %s", err)
			return err
		}

		sniffer.afpacketHandle, err = newAfpacketHandle(
			sniffer.config.Device,
			szFrame,
			szBlock,
			numBlocks,
			500*time.Millisecond)
		if err != nil {
			logp.Err("sniffer %s", err)
			return err
		}

		err = sniffer.afpacketHandle.SetBPFFilter(sniffer.filter)
		if err != nil {
			return fmt.Errorf("SetBPFFilter failed: %s", err)
		}

		sniffer.DataSource = gopacket.PacketDataSource(sniffer.afpacketHandle)

	default:
		return fmt.Errorf("Unknown sniffer type: %s", sniffer.config.Type)
	}

	return nil
}

func (sniffer *SnifferSetup) Reopen() error {
	var err error

	if sniffer.config.Type != "pcap" || sniffer.config.File == "" {
		return fmt.Errorf("Reopen is only possible for files")
	}

	sniffer.pcapHandle.Close()
	sniffer.pcapHandle, err = pcap.OpenOffline(sniffer.config.File)
	if err != nil {
		return err
	}

	sniffer.DataSource = gopacket.PacketDataSource(sniffer.pcapHandle)

	return nil
}

func (sniffer *SnifferSetup) Datalink() layers.LinkType {
	if sniffer.config.Type == "pcap" {
		return sniffer.pcapHandle.LinkType()
	}
	return layers.LinkTypeEthernet
}

func (sniffer *SnifferSetup) Init(testMode bool, filter string, factory WorkerFactory, interfaces *config.InterfacesConfig) error {
	var err error

	if !testMode {
		sniffer.filter = filter
		logp.Debug("sniffer", "BPF filter: '%s'", sniffer.filter)
		err = sniffer.setFromConfig(interfaces)
		if err != nil {
			return fmt.Errorf("Error creating sniffer: %v", err)
		}
	}

	if len(interfaces.File) == 0 {
		if interfaces.Device == "any" {
			// OS X or Windows
			if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
				return fmt.Errorf("any interface is not supported on %s", runtime.GOOS)
			}
		}
	}

	sniffer.worker, err = factory(sniffer.Datalink())
	if err != nil {
		return fmt.Errorf("Error creating decoder: %v", err)
	}

	if sniffer.config.Dumpfile != "" {
		p, err := pcap.OpenDead(sniffer.Datalink(), 65535)
		if err != nil {
			return err
		}
		sniffer.dumper, err = p.NewDumper(sniffer.config.Dumpfile)
		if err != nil {
			return err
		}
	}

	sniffer.isAlive = true

	return nil
}

func (sniffer *SnifferSetup) Run() error {
	counter := 0
	loopCount := 1
	var lastPktTime *time.Time
	var retError error

	for sniffer.isAlive {
		if sniffer.config.OneAtATime {
			fmt.Println("Press enter to read packet")
			fmt.Scanln()
		}

		data, ci, err := sniffer.DataSource.ReadPacketData()

		if err == pcap.NextErrorTimeoutExpired || err == syscall.EINTR {
			//logp.Debug("sniffer", "Interrupted")
			continue
		}

		//logp.Debug("sniffer", "End of file")
		if err == io.EOF {
			logp.Debug("sniffer", "End of file")
			loopCount++
			if sniffer.config.Loop > 0 && loopCount > sniffer.config.Loop {
				// give a bit of time to the publish goroutine
				// to flush
				time.Sleep(300 * time.Millisecond)
				sniffer.isAlive = false
				continue
			}

			logp.Debug("sniffer", "Reopening the file")
			err = sniffer.Reopen()
			if err != nil {
				retError = fmt.Errorf("Error reopening file: %s", err)
				sniffer.isAlive = false
				continue
			}
			lastPktTime = nil
			continue
		}

		if err != nil {
			retError = fmt.Errorf("Sniffing error: %s", err)
			sniffer.isAlive = false
			continue
		}

		if len(data) == 0 {
			// Empty packet, probably timeout from afpacket
			continue
		}

		if sniffer.config.File != "" {
			if lastPktTime != nil && !sniffer.config.TopSpeed {
				sleep := ci.Timestamp.Sub(*lastPktTime)
				if sleep > 0 {
					time.Sleep(sleep)
				} else {
					logp.Warn("Time in pcap went backwards: %d", sleep)
				}
			}
			_lastPktTime := ci.Timestamp
			lastPktTime = &_lastPktTime
			if !sniffer.config.TopSpeed {
				// Overwrite what we get from the pcap
				ci.Timestamp = time.Now()
			}
		}
		counter++

		if sniffer.dumper != nil {
			sniffer.dumper.WritePacketData(data, ci)
		}
		if counter%1024 == 0 {
			logp.Debug("sniffer", "Packet number: %d", counter)
		}

		sniffer.worker.OnPacket(data, &ci)
	}

	logp.Info("Input finish. Processed %d packets. Have a nice day!", counter)

	if sniffer.dumper != nil {
		sniffer.dumper.Close()
	}
	return retError
}

func (sniffer *SnifferSetup) Close() error {
	switch sniffer.config.Type {
	case "pcap":
		sniffer.pcapHandle.Close()
	case "af_packet":
		sniffer.afpacketHandle.Close()
	}
	return nil
}

func (sniffer *SnifferSetup) Stop() error {
	sniffer.isAlive = false
	return nil
}

func (sniffer *SnifferSetup) IsAlive() bool {
	return sniffer.isAlive
}
