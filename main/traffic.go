/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	traffic.go: Target management, UAT downlink message processing, 1090ES source input, GDL90 traffic reports.
*/

package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

//-0b2b48fe3aef1f88621a0856110a31c01105c4e6c4e6c40a9a820300000000000000;rs=7;

/*

HDR:
 MDB Type:          1
 Address:           2B48FE (TIS-B track file address)
SV:
 NIC:               6
 Latitude:          +41.4380
 Longitude:         -84.1056
 Altitude:          2300 ft (barometric)
 N/S velocity:      -65 kt
 E/W velocity:      -98 kt
 Track:             236
 Speed:             117 kt
 Vertical rate:     0 ft/min (from barometric altitude)
 UTC coupling:      no
 TIS-B site ID:     1
MS:
 Emitter category:  No information
 Callsign:          unavailable
 Emergency status:  No emergency
 UAT version:       2
 SIL:               2
 Transmit MSO:      38
 NACp:              8
 NACv:              1
 NICbaro:           0
 Capabilities:
 Active modes:
 Target track type: true heading
AUXSV:
 Sec. altitude:     unavailable

*/

const (
	TRAFFIC_SOURCE_1090ES = 1
	TRAFFIC_SOURCE_UAT    = 2
	TARGET_TYPE_MODE_S    = 0
	TARGET_TYPE_ADSB      = 1
	TARGET_TYPE_ADSR      = 2
	// Assign next type to UAT messages with address qualifier == 2
	// (code corresponds to any UAT GBT targets with Mode S addresses.
	// These will be displayed with the airplane icon on the traffic UI page.
	// If we see a proper emitter category and NIC > 7, they'll be reassigned to TYPE_ADSR.
	TARGET_TYPE_TISB_S = 3
	TARGET_TYPE_TISB   = 4
)

type TrafficInfo struct {
	Icao_addr           uint32
	Reg                 string    // Registration. Calculated from Icao_addr for civil aircraft of US registry.
	Tail                string    // Callsign. Transmitted by aircraft.
	Emitter_category    uint8     // Formatted using GDL90 standard, e.g. in a Mode ES report, A7 becomes 0x07, B0 becomes 0x08, etc.
	OnGround            bool      // Air-ground status. On-ground is "true".
	Addr_type           uint8     // UAT address qualifier. Used by GDL90 format, so translations for ES TIS-B/ADS-R are needed.
	TargetType          uint8     // types decribed in const above
	SignalLevel         float64   // Signal level, dB RSSI.
	Squawk              int       // Squawk code
	Position_valid      bool      //TODO: set when position report received. Unset after n seconds?
	Lat                 float32   // decimal degrees, north positive
	Lng                 float32   // decimal degrees, east positive
	Alt                 int32     // Pressure altitude, feet
	GnssDiffFromBaroAlt int32     // GNSS altitude above WGS84 datum. Reported in TC 20-22 messages
	AltIsGNSS           bool      // Pressure alt = 0; GNSS alt = 1
	NIC                 int       // Navigation Integrity Category.
	NACp                int       // Navigation Accuracy Category for Position.
	Track               uint16    // degrees true
	Speed               uint16    // knots
	Speed_valid         bool      // set when speed report received.
	Vvel                int16     // feet per minute
	Timestamp           time.Time // timestamp of traffic message, UTC
	PriorityStatus      uint8     // Emergency or priority code as defined in GDL90 spec, DO-260B (Type 28 msg) and DO-282B

	// Parameters starting at 'Age' are calculated from last message receipt on each call of sendTrafficUpdates().
	// Mode S transmits position and track in separate messages, and altitude can also be
	// received from interrogations.
	Age                  float64   // Age of last valid position fix, seconds ago.
	AgeLastAlt           float64   // Age of last altitude message, seconds ago.
	Last_seen            time.Time // Time of last position update (stratuxClock). Used for timing out expired data.
	Last_alt             time.Time // Time of last altitude update (stratuxClock).
	Last_GnssDiff        time.Time // Time of last GnssDiffFromBaroAlt update (stratuxClock).
	Last_GnssDiffAlt     int32     // Altitude at last GnssDiffFromBaroAlt update.
	Last_speed           time.Time // Time of last velocity and track update (stratuxClock).
	Last_source          uint8     // Last frequency on which this target was received.
	ExtrapolatedPosition bool      //TODO: True if Stratux is "coasting" the target from last known position.
	BearingDist_valid    bool      // set when bearing and distance information is valid
	Bearing              float64   // Bearing in degrees true to traffic from ownship, if it can be calculated. Units: degrees.
	Distance             float64   // Distance to traffic from ownship, if it can be calculated. Units: meters.
	//FIXME: Rename variables for consistency, especially "Last_".
}

type dump1090Data struct {
	Icao_addr           uint32
	DF                  int     // Mode S downlink format.
	CA                  int     // Lowest 3 bits of first byte of Mode S message (DF11 and DF17 capability; DF18 control field, zero for all other DF types)
	TypeCode            int     // Mode S type code
	SubtypeCode         int     // Mode S subtype code
	SBS_MsgType         int     // type of SBS message (used in "old" 1090 parsing)
	SignalLevel         float64 // Decimal RSSI (0-1 nominal) as reported by dump1090-mutability. Convert to dB RSSI before setting in TrafficInfo.
	Tail                *string
	Squawk              *int // 12-bit squawk code in octal format
	Emitter_category    *int
	OnGround            *bool
	Lat                 *float32
	Lng                 *float32
	Position_valid      bool
	NACp                *int
	Alt                 *int
	AltIsGNSS           bool   //
	GnssDiffFromBaroAlt *int16 // GNSS height above baro altitude in feet; valid range is -3125 to 3125. +/- 3138 indicates larger difference.
	Vvel                *int16
	Speed_valid         bool
	Speed               *uint16
	Track               *uint16
	Timestamp           time.Time // time traffic last seen, UTC
}

type esmsg struct {
	TimeReceived time.Time
	Data         string
}

var traffic map[uint32]TrafficInfo
var trafficMutex *sync.Mutex
var seenTraffic map[uint32]bool // Historical list of all ICAO addresses seen.

var OwnshipTrafficInfo TrafficInfo

func convertFeetToMeters(feet float32) float32 {
	return feet * 0.3048
}

func convertMetersToFeet(meters float32) float32 {
	return meters / 0.3048
}

func cleanupOldEntries() {
	for icao_addr, ti := range traffic {
		if stratuxClock.Since(ti.Last_seen) > 60*time.Second { // keep it in the database for up to 60 seconds, so we don't lose tail number, etc...
			delete(traffic, icao_addr)
		}
	}
}

func sendTrafficUpdates() {
	trafficMutex.Lock()
	defer trafficMutex.Unlock()
	cleanupOldEntries()

	// Summarize number of UAT and 1090ES traffic targets for reports that follow.
	globalStatus.UAT_traffic_targets_tracking = 0
	globalStatus.ES_traffic_targets_tracking = 0
	for _, traf := range traffic {
		switch traf.Last_source {
		case TRAFFIC_SOURCE_1090ES:
			globalStatus.ES_traffic_targets_tracking++
		case TRAFFIC_SOURCE_UAT:
			globalStatus.UAT_traffic_targets_tracking++
		}
	}

	msgs := make([][]byte, 1)
	if globalSettings.DEBUG && (stratuxClock.Time.Second()%15) == 0 {
		log.Printf("List of all aircraft being tracked:\n")
		log.Printf("==================================================================\n")
	}
	code, _ := strconv.ParseInt(globalSettings.OwnshipModeS, 16, 32)
	for icao, ti := range traffic { // ForeFlight 7.5 chokes at ~1000-2000 messages depending on iDevice RAM. Practical limit likely around ~500 aircraft without filtering.
		if isGPSValid() {
			// func distRect(lat1, lon1, lat2, lon2 float64) (dist, bearing, distN, distE float64) {
			dist, bearing := distance(float64(mySituation.GPSLatitude), float64(mySituation.GPSLongitude), float64(ti.Lat), float64(ti.Lng))
			ti.Distance = dist
			ti.Bearing = bearing
			ti.BearingDist_valid = true
		} else {
			ti.Distance = 0
			ti.Bearing = 0
			ti.BearingDist_valid = false
		}
		ti.Age = stratuxClock.Since(ti.Last_seen).Seconds()
		ti.AgeLastAlt = stratuxClock.Since(ti.Last_alt).Seconds()

		// DEBUG: Print the list of all tracked targets (with data) to the log every 15 seconds if "DEBUG" option is enabled
		if globalSettings.DEBUG && (stratuxClock.Time.Second()%15) == 0 {
			s_out, err := json.Marshal(ti)
			if err != nil {
				log.Printf("Error generating output: %s\n", err.Error())
			} else {
				log.Printf("%X => %s\n", ti.Icao_addr, string(s_out))
			}
			// end of debug block
		}
		traffic[icao] = ti // write the updated ti back to the map
		//log.Printf("Traffic age of %X is %f seconds\n",icao,ti.Age)
		if ti.Age > 2 { // if nothing polls an inactive ti, it won't push to the webUI, and its Age won't update.
			trafficUpdate.SendJSON(ti)
		}
		if ti.Position_valid && ti.Age < 6 { // ... but don't pass stale data to the EFB.
			//TODO: Coast old traffic? Need to determine how FF, WingX, etc deal with stale targets.
			logTraffic(ti) // only add to the SQLite log if it's not stale

			if ti.Icao_addr == uint32(code) {
				if globalSettings.DEBUG {
					log.Printf("Ownship target detected for code %X\n", code)
				}
				//				OwnshipTrafficInfo = ti
			} else {
				cur_n := len(msgs) - 1
				if len(msgs[cur_n]) >= 35 {
					// Batch messages into packets with at most 35 traffic reports
					//  to keep each packet under 1KB.
					cur_n++
					msgs = append(msgs, make([]byte, 0))
				}
				msgs[cur_n] = append(msgs[cur_n], makeTrafficReportMsg(ti)...)
				
				var trafficCallsign string
				if len(ti.Tail) > 0 {
					trafficCallsign = ti.Tail
				} else {
					trafficCallsign = fmt.Sprintf("%X_%d", ti.Icao_addr, ti.Squawk)
				}

				// send traffic message to X-Plane
				sendXPlane(createXPlaneTrafficMsg(ti.Icao_addr, ti.Lat, ti.Lng, ti.Alt, uint32(ti.Speed), int32(ti.Vvel), ti.OnGround, uint32(ti.Track), trafficCallsign), false)
			}
		}
	}

	for i := 0; i < len(msgs); i++ {
		msg := msgs[i]
		if len(msg) > 0 {
			sendGDL90(msg, false)
		}
	}
}

// Send update to attached JSON client.
func registerTrafficUpdate(ti TrafficInfo) {
	//logTraffic(ti) // moved to sendTrafficUpdates() to reduce SQLite log size
	/*
		if !ti.Position_valid { // Don't send unless a valid position exists.
			return
		}
	*/ // Send all traffic to the websocket and let JS sort it out. This will provide user indication of why they see 1000 ES messages and no traffic.
	trafficUpdate.SendJSON(ti)
}

func isTrafficAlertable(ti TrafficInfo) bool {
	// Set alert bit if possible and traffic is within some threshold
	// TODO: Could be more intelligent, taking into account headings etc.
	if !ti.BearingDist_valid {
		// If not able to calculate the distance to the target, let the alert bit be set always.
		return true
	}
	if ti.BearingDist_valid &&
		ti.Distance < 3704 { // 3704 meters, 2 nm.
		return true
	}

	return false
}

func makeTrafficReportMsg(ti TrafficInfo) []byte {
	msg := make([]byte, 28)
	// See p.16.
	msg[0] = 0x14 // Message type "Traffic Report".

	// Address type
	msg[1] = ti.Addr_type

	// Set alert if needed
	if isTrafficAlertable(ti) {
		// Set the alert bit.  See pg. 18 of GDL90 ICD
		msg[1] |= 0x10
	}

	// ICAO Address.
	msg[2] = byte((ti.Icao_addr & 0x00FF0000) >> 16)
	msg[3] = byte((ti.Icao_addr & 0x0000FF00) >> 8)
	msg[4] = byte((ti.Icao_addr & 0x000000FF))

	lat := float32(ti.Lat)
	tmp := makeLatLng(lat)

	msg[5] = tmp[0] // Latitude.
	msg[6] = tmp[1] // Latitude.
	msg[7] = tmp[2] // Latitude.

	lng := float32(ti.Lng)
	tmp = makeLatLng(lng)

	msg[8] = tmp[0]  // Longitude.
	msg[9] = tmp[1]  // Longitude.
	msg[10] = tmp[2] // Longitude.

	// Altitude: OK
	// GDL 90 Data Interface Specification examples:
	// where 1,000 foot offset and 25 foot resolution (1,000 / 25 = 40)
	//    -1,000 feet               0x000
	//    0 feet                    0x028
	//    +1000 feet                0x050
	//    +101,350 feet             0xFFE
	//    Invalid or unavailable    0xFFF
	//
	// Algo example at: https://play.golang.org/p/VXCckSdsvT
	//
	var alt int16
	if ti.Alt < -1000 || ti.Alt > 101350 {
		alt = 0x0FFF
	} else {
		// output guaranteed to be between 0x0000 and 0x0FFE
		alt = int16((ti.Alt / 25) + 40)
	}
	msg[11] = byte((alt & 0xFF0) >> 4) // Altitude.
	msg[12] = byte((alt & 0x00F) << 4)

	// "m" field. Lower four bits define indicator bits:
	// - - 0 0   "tt" (msg[17]) is not valid
	// - - 0 1   "tt" is true track
	// - - 1 0   "tt" is magnetic heading
	// - - 1 1   "tt" is true heading
	// - 0 - -   Report is updated (current data)
	// - 1 - -   Report is extrapolated
	// 0 - - -   On ground
	// 1 - - -   Airborne

	// Define tt type / validity
	if ti.Speed_valid {
		msg[12] = msg[12] | 0x01 // assume true track
	}

	if ti.ExtrapolatedPosition {
		msg[12] = msg[12] | 0x04
	}

	if !ti.OnGround {
		msg[12] = msg[12] | 0x08 // Airborne.
	}

	// Position containment / navigational accuracy
	msg[13] = ((byte(ti.NIC) << 4) & 0xF0) | (byte(ti.NACp) & 0x0F)

	// Horizontal velocity (speed).

	msg[14] = byte((ti.Speed & 0x0FF0) >> 4)
	msg[15] = byte((ti.Speed & 0x000F) << 4)

	// Vertical velocity.
	vvel := ti.Vvel / 64 // 64fpm resolution.
	msg[15] = msg[15] | byte((vvel&0x0F00)>>8)
	msg[16] = byte(vvel & 0x00FF)

	// Track.
	trk := uint8(float32(ti.Track) / TRACK_RESOLUTION) // Resolution is ~1.4 degrees.
	msg[17] = byte(trk)

	msg[18] = ti.Emitter_category

	// msg[19] to msg[26] are "call sign" (tail).
	for i := 0; i < len(ti.Tail) && i < 8; i++ {
		c := byte(ti.Tail[i])
		if c != 20 && !((c >= 48) && (c <= 57)) && !((c >= 65) && (c <= 90)) && c != 'e' && c != 'u' && c != 'a' && c != 'r' && c != 't' { // See p.24, FAA ref.
			c = byte(20)
		}
		msg[19+i] = c
	}

	//msg[27] is priority / emergency status per GDL90 spec (DO260B and DO282B are same codes)
	msg[27] = ti.PriorityStatus << 4

	return prepareMessage(msg)
}

// parseDownlinkReport decodes a UAT downlink message to extract identity, state vector, and mode status data.
// Decoded data is used to update a TrafficInfo object, keyed to the 24-bit ICAO code contained in the
// downlink message.
// Inputs are a checksum-verified hex string corresponding to the 18 or 34-byte UAT
// message, and an int representing UAT signal amplitude (0-1000).
func parseDownlinkReport(s string, signalLevel int) {

	var ti TrafficInfo
	s = s[1:]
	frame := make([]byte, len(s)/2)
	hex.Decode(frame, []byte(s))

	// Extract header
	msg_type := (uint8(frame[0]) >> 3) & 0x1f
	addr_type := uint8(frame[0]) & 0x07
	icao_addr := (uint32(frame[1]) << 16) | (uint32(frame[2]) << 8) | uint32(frame[3])

	trafficMutex.Lock()
	defer trafficMutex.Unlock()

	// Retrieve previous information on this ICAO code.
	if val, ok := traffic[icao_addr]; ok { // if we've already seen it, copy it in to do updates as it may contain some useful information like "tail" from 1090ES.
		ti = val
		//log.Printf("Existing target %X imported for UAT update\n", icao_addr)
	} else {
		//log.Printf("New target %X created for UAT update\n", icao_addr)
		ti.Last_seen = stratuxClock.Time // need to initialize to current stratuxClock so it doesn't get cut before we have a chance to populate a position message
		ti.Icao_addr = icao_addr
		ti.ExtrapolatedPosition = false

		thisReg, validReg := icao2reg(icao_addr)
		if validReg {
			ti.Reg = thisReg
			ti.Tail = thisReg
		}
	}

	ti.Addr_type = addr_type

	var uat_version byte // sent as part of MS element, byte 24

	// Extract parameters from Mode Status elements, if available.
	if msg_type == 1 || msg_type == 3 {

		// Determine UAT message version. This is needed for some capability decoding and is useful for debugging.
		uat_version = (frame[23] >> 2) & 0x07

		// Extract emitter category.
		v := (uint16(frame[17]) << 8) | (uint16(frame[18]))
		ti.Emitter_category = uint8((v / 1600) % 40)

		// Decode callsign or Flight Plan ID (i.e. squawk code)
		// If the CSID bit (byte 27, bit 7) is set to 1, all eight characters
		// encoded in bytes 18-23 represent callsign.
		// If the CSID bit is set to 0, the first four characters encoded in bytes 18-23
		// represent the Mode A squawk code.

		csid := (frame[26] >> 1) & 0x01

		if csid == 1 { // decode as callsign
			base40_alphabet := string("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ  ..")
			tail := ""

			v := (uint16(frame[17]) << 8) | uint16(frame[18])
			tail += string(base40_alphabet[(v/40)%40])
			tail += string(base40_alphabet[v%40])
			v = (uint16(frame[19]) << 8) | uint16(frame[20])
			tail += string(base40_alphabet[(v/1600)%40])
			tail += string(base40_alphabet[(v/40)%40])
			tail += string(base40_alphabet[v%40])
			v = (uint16(frame[21]) << 8) | uint16(frame[22])
			tail += string(base40_alphabet[(v/1600)%40])
			tail += string(base40_alphabet[(v/40)%40])
			tail += string(base40_alphabet[v%40])
			tail = strings.Trim(tail, " ")
			ti.Tail = tail

		} else if uat_version >= 2 { // decode as Mode 3/A code, if UAT version is at least 2
			v := (uint16(frame[17]) << 8) | uint16(frame[18])
			squawk_a := (v / 40) % 40
			squawk_b := v % 40
			v = (uint16(frame[19]) << 8) | uint16(frame[20])
			squawk_c := (v / 1600) % 40
			squawk_d := (v / 40) % 40
			squawk := 1000*squawk_a + 100*squawk_b + 10*squawk_c + squawk_d
			ti.Squawk = int(squawk)
		}

		ti.NACp = int((frame[25] >> 4) & 0x0F)
		ti.PriorityStatus = (frame[23] >> 5) & 0x07

		// Following section is future-use for debugging and / or additional status info on UAT traffic. Message parsing needs testing.

		if globalSettings.DEBUG {
			//declaration for mode status flags -- parse for debug logging
			var status_sil byte
			//var status_transmit_mso byte
			var status_sda byte
			var status_nacv byte
			//var status_nicbaro byte
			//var status_sil_supp byte
			//var status_geom_vert_acc byte
			//var status_sa_flag byte
			var capability_uat_in bool
			var capability_1090_in bool
			//var capability_tcas bool
			//var capability_cdti bool
			//var opmode_tcas_active bool
			//var opmode_ident_active bool
			//var opmode_rec_atc_serv bool

			// these are present in v1 and v2 messages
			status_sil = frame[23] & 0x03
			//status_transmit_mso = frame[24] >> 2
			status_nacv = (frame[25] >> 1) & 0x07
			//status_nicbaro = frame[25] & 0x01

			// other status and capability bits are different between v1 and v2
			if uat_version == 2 {
				status_sda = frame[24] & 0x03
				capability_uat_in = (frame[26] >> 7) != 0
				capability_1090_in = ((frame[26] >> 6) & 0x01) != 0
				//capability_tcas = ((frame[26] >> 5) & 0x01) != 0
				//opmode_tcas_active = ((frame[26] >> 4) & 0x01) != 0
				//opmode_ident_active = ((frame[26] >> 3) & 0x01) != 0
				//opmode_rec_atc_serv = ((frame[26] >> 2) & 0x01) != 0
				//status_sil_supp = frame[26] & 0x01
				//status_geom_vert_acc = (frame[27] >> 6) & 0x03
				//status_sa_flag = (frame[27] >> 5) & 0x01

			} else if uat_version == 1 {
				//capability_cdti = (frame[26] >> 7) != 0
				//capability_tcas = ((frame[26] >> 6) & 0x01) != 0
				//opmode_tcas_active = ((frame[26] >> 5) & 0x01) != 0
				//opmode_ident_active = ((frame[26] >> 4) & 0x01) != 0
				//opmode_rec_atc_serv = ((frame[26] >> 3) & 0x01) != 0
			}

			log.Printf("Supplemental UAT Mode Status for %06X: Version = %d; SIL = %d; SDA = %d; NACv = %d; 978 In = %v; 1090 In = %v\n", icao_addr, uat_version, status_sil, status_sda, status_nacv, capability_uat_in, capability_1090_in)
		}
	}

	ti.NIC = int(frame[11] & 0x0F)

	var power float64
	if signalLevel > 0 {
		power = 20 * (math.Log10(float64(signalLevel) / 1000)) // reported amplitude is 0-1000. Normalize to max = 1 and do amplitude dB calculation (20 dB per decade)
	} else {
		power = -999
	}
	//log.Printf("%s (%X) seen with amplitude of %d, corresponding to normalized power of %f.2 dB\n",ti.Tail,ti.Icao_addr,signalLevel,power)

	ti.SignalLevel = power

	if ti.Addr_type == 0 {
		ti.TargetType = TARGET_TYPE_ADSB
	} else if ti.Addr_type == 3 {
		ti.TargetType = TARGET_TYPE_TISB
	} else if ti.Addr_type == 6 {
		ti.TargetType = TARGET_TYPE_ADSR
	} else if ti.Addr_type == 2 {
		ti.TargetType = TARGET_TYPE_TISB_S
		if (ti.NIC >= 7) && (ti.Emitter_category > 0) { // If NIC is sufficiently high and emitter type is transmitted, we'll assume it's ADS-R.
			ti.TargetType = TARGET_TYPE_ADSR
		}
	}

	// This is a hack to show the source of the traffic on moving maps.
	if globalSettings.DisplayTrafficSource {
		type_code := " "
		switch ti.TargetType {
		case TARGET_TYPE_ADSB:
			type_code = "a"
		case TARGET_TYPE_ADSR, TARGET_TYPE_TISB_S:
			type_code = "r"
		case TARGET_TYPE_TISB:
			type_code = "t"
		}

		if len(ti.Tail) == 0 {
			ti.Tail = "u" + type_code
		} else if len(ti.Tail) < 7 && ti.Tail[0] != 'e' && ti.Tail[0] != 'u' {
			ti.Tail = "u" + type_code + ti.Tail
		} else if len(ti.Tail) == 7 && ti.Tail[0] != 'e' && ti.Tail[0] != 'u' {
			ti.Tail = "u" + type_code + ti.Tail[1:]
		} else if len(ti.Tail) > 1 { // bounds checking
			ti.Tail = "u" + type_code + ti.Tail[2:]
		}
	}
	raw_lat := (uint32(frame[4]) << 15) | (uint32(frame[5]) << 7) | (uint32(frame[6]) >> 1)
	raw_lon := ((uint32(frame[6]) & 0x01) << 23) | (uint32(frame[7]) << 15) | (uint32(frame[8]) << 7) | (uint32(frame[9]) >> 1)

	lat := float32(0.0)
	lng := float32(0.0)

	position_valid := false
	if /*(ti.NIC != 0) && */ (raw_lat != 0) && (raw_lon != 0) { // pass all traffic, and let the display determine if it will show NIC == 0. This will allow misconfigured or uncertified / portable emitters to be seen.
		position_valid = true
		lat = float32(raw_lat) * 360.0 / 16777216.0
		if lat > 90 {
			lat = lat - 180
		}
		lng = float32(raw_lon) * 360.0 / 16777216.0
		if lng > 180 {
			lng = lng - 360
		}
	}
	ti.Position_valid = position_valid
	if ti.Position_valid {
		ti.Lat = lat
		ti.Lng = lng
		if isGPSValid() {
			ti.Distance, ti.Bearing = distance(float64(mySituation.GPSLatitude), float64(mySituation.GPSLongitude), float64(ti.Lat), float64(ti.Lng))
		}
		ti.Last_seen = stratuxClock.Time
		ti.ExtrapolatedPosition = false
	}

	raw_alt := (int32(frame[10]) << 4) | ((int32(frame[11]) & 0xf0) >> 4)
	alt_geo := false // Default case (i.e. 'false') is barometric
	alt := int32(0)
	if raw_alt != 0 {
		alt_geo = (uint8(frame[9]) & 1) != 0
		alt = ((raw_alt - 1) * 25) - 1000
	}
	ti.Alt = alt
	ti.AltIsGNSS = alt_geo
	ti.Last_alt = stratuxClock.Time

	//OK.
	//	fmt.Printf("%d, %t, %f, %f, %t, %d\n", nic, position_valid, lat, lng, alt_geo, alt)

	airground_state := (uint8(frame[12]) >> 6) & 0x03
	//OK.
	//	fmt.Printf("%d\n", airground_state)

	ns_vel := int32(0) // int16 won't work. Worst case (supersonic), we need 26 bits (25 bits + sign) for root sum of squares speed calculation
	ew_vel := int32(0)
	track := uint16(0)
	speed_valid := false
	speed := uint16(0)
	vvel := int16(0)
	//	vvel_geo := false
	if airground_state == 0 || airground_state == 1 { // Subsonic. Supersonic.
		ti.OnGround = false
		// N/S velocity.
		ns_vel_valid := false
		ew_vel_valid := false
		raw_ns := ((int16(frame[12]) & 0x1f) << 6) | ((int16(frame[13]) & 0xfc) >> 2)
		if (raw_ns & 0x3ff) != 0 {
			ns_vel_valid = true
			ns_vel = int32((raw_ns & 0x3ff) - 1)
			if (raw_ns & 0x400) != 0 {
				ns_vel = 0 - ns_vel
			}
			if airground_state == 1 { // Supersonic.
				ns_vel = ns_vel * 4
			}
		}
		// E/W velocity.
		raw_ew := ((int16(frame[13]) & 0x03) << 9) | (int16(frame[14]) << 1) | ((int16(frame[15] & 0x80)) >> 7)
		if (raw_ew & 0x3ff) != 0 {
			ew_vel_valid = true
			ew_vel = int32((raw_ew & 0x3ff) - 1)
			if (raw_ew & 0x400) != 0 {
				ew_vel = 0 - ew_vel
			}
			if airground_state == 1 { // Supersonic.
				ew_vel = ew_vel * 4
			}
		}
		if ns_vel_valid && ew_vel_valid {
			if ns_vel != 0 || ew_vel != 0 {
				//TODO: Track type
				track = uint16((360 + 90 - (int16(math.Atan2(float64(ns_vel), float64(ew_vel)) * 180 / math.Pi))) % 360)
			}
			speed_valid = true
			speed = uint16(math.Sqrt(float64((ns_vel * ns_vel) + (ew_vel * ew_vel))))
		}

		// Vertical velocity.
		raw_vvel := ((int16(frame[15]) & 0x7f) << 4) | ((int16(frame[16]) & 0xf0) >> 4)
		if (raw_vvel & 0x1ff) != 0 {
			//			vvel_geo = (raw_vvel & 0x400) == 0
			vvel = ((raw_vvel & 0x1ff) - 1) * 64
			if (raw_vvel & 0x200) != 0 {
				vvel = 0 - vvel
			}
		}
	} else if airground_state == 2 { // Ground vehicle.
		ti.OnGround = true
		raw_gs := ((uint16(frame[12]) & 0x1f) << 6) | ((uint16(frame[13]) & 0xfc) >> 2)
		if raw_gs != 0 {
			speed_valid = true
			speed = ((raw_gs & 0x3ff) - 1)
		}

		raw_track := ((uint16(frame[13]) & 0x03) << 9) | (uint16(frame[14]) << 1) | ((uint16(frame[15]) & 0x80) >> 7)
		//tt := ((raw_track & 0x0600) >> 9)
		//FIXME: tt == 1 TT_TRACK. tt == 2 TT_MAG_HEADING. tt == 3 TT_TRUE_HEADING.
		track = uint16((raw_track & 0x1ff) * 360 / 512)

		// Dimensions of vehicle - skip.
	}

	ti.Track = track
	ti.Speed = speed
	ti.Vvel = vvel
	ti.Speed_valid = speed_valid
	if ti.Speed_valid {
		ti.Last_speed = stratuxClock.Time
	}

	//	fmt.Printf("ns_vel %d, ew_vel %d, track %d, speed_valid %t, speed %d, vvel_geo %t, vvel %d\n", ns_vel, ew_vel, track, speed_valid, speed, vvel_geo, vvel)

	/*
		utc_coupled := false
		tisb_site_id := uint8(0)

		if (uint8(frame[0]) & 7) == 2 || (uint8(frame[0]) & 7) == 3 { //TODO: Meaning?
			tisb_site_id = uint8(frame[16]) & 0x0f
		} else {
			utc_coupled = (uint8(frame[16]) & 0x08) != 0
		}
	*/

	//	fmt.Printf("tisb_site_id %d, utc_coupled %t\n", tisb_site_id, utc_coupled)

	ti.Timestamp = time.Now()

	ti.Last_source = TRAFFIC_SOURCE_UAT

	traffic[ti.Icao_addr] = ti
	registerTrafficUpdate(ti)
	seenTraffic[ti.Icao_addr] = true // Mark as seen.
}

func esListen() {
	for {
		if !globalSettings.ES_Enabled && !globalSettings.Ping_Enabled {
			time.Sleep(1 * time.Second) // Don't do much unless ES is actually enabled.
			continue
		}
		dump1090Addr := "127.0.0.1:30006"
		inConn, err := net.Dial("tcp", dump1090Addr)
		if err != nil { // Local connection failed.
			time.Sleep(1 * time.Second)
			continue
		}
		rdr := bufio.NewReader(inConn)
		for globalSettings.ES_Enabled || globalSettings.Ping_Enabled {
			//log.Printf("ES enabled. Ready to read next message from dump1090\n")
			buf, err := rdr.ReadString('\n')
			//log.Printf("String read from dump1090\n")
			if err != nil { // Must have disconnected?
				break
			}
			buf = strings.Trim(buf, "\r\n")

			// Log the message to the message counter in any case.
			var thisMsg msg
			thisMsg.MessageClass = MSGCLASS_ES
			thisMsg.TimeReceived = stratuxClock.Time
			thisMsg.Data = buf
			MsgLog = append(MsgLog, thisMsg)

			var eslog esmsg
			eslog.TimeReceived = stratuxClock.Time
			eslog.Data = buf
			logESMsg(eslog) // log raw dump1090:30006 output to SQLite log

			var newTi *dump1090Data
			err = json.Unmarshal([]byte(buf), &newTi)
			if err != nil {
				log.Printf("can't read ES traffic information from %s: %s\n", buf, err.Error())
				continue
			}

			if newTi.Icao_addr == 0x07FFFFFF { // used to signal heartbeat
				if globalSettings.DEBUG {
					log.Printf("No traffic last 60 seconds. Heartbeat message from dump1090: %s\n", buf)
				}
				continue // don't process heartbeat messages
			}

			if (newTi.Icao_addr & 0x01000000) != 0 { // bit 25 used by dump1090 to signal non-ICAO address
				newTi.Icao_addr = newTi.Icao_addr & 0x00FFFFFF
				if globalSettings.DEBUG {
					log.Printf("Non-ICAO address %X sent by dump1090. This is typical for TIS-B.\n", newTi.Icao_addr)
				}
			}
			icao := uint32(newTi.Icao_addr)
			var ti TrafficInfo

			trafficMutex.Lock()

			// Retrieve previous information on this ICAO code.
			if val, ok := traffic[icao]; ok { // if we've already seen it, copy it in to do updates
				ti = val
				//log.Printf("Existing target %X imported for ES update\n", icao)
			} else {
				//log.Printf("New target %X created for ES update\n",newTi.Icao_addr)
				ti.Last_seen = stratuxClock.Time // need to initialize to current stratuxClock so it doesn't get cut before we have a chance to populate a position message
				ti.Last_alt = stratuxClock.Time  // ditto.
				ti.Icao_addr = icao
				ti.ExtrapolatedPosition = false
				ti.Last_source = TRAFFIC_SOURCE_1090ES

				thisReg, validReg := icao2reg(icao)
				if validReg {
					ti.Reg = thisReg
					ti.Tail = thisReg
				}
			}

			if newTi.SignalLevel > 0 {
				ti.SignalLevel = 10 * math.Log10(newTi.SignalLevel)
			} else {
				ti.SignalLevel = -999
			}

			// generate human readable summary of message types for debug
			//TODO: Use for ES message statistics?
			/*
				var s1 string
				if newTi.DF == 17 {
					s1 = "ADS-B"
				}
				if newTi.DF == 18 {
					s1 = "ADS-R / TIS-B"
				}

				if newTi.DF == 4 || newTi.DF == 20 {
					s1 = "Surveillance, Alt. Reply"
				}

				if newTi.DF == 5 || newTi.DF == 21 {
					s1 = "Surveillance, Ident. Reply"
				}

				if newTi.DF == 11 {
					s1 = "All-call Reply"
				}

				if newTi.DF == 0 {
					s1 = "Short Air-Air Surv."
				}

				if newTi.DF == 16 {
					s1 = "Long Air-Air Surv."
				}
			*/
			//log.Printf("Mode S message from icao=%X, DF=%02d, CA=%02d, TC=%02d (%s)\n", ti.Icao_addr, newTi.DF, newTi.CA, newTi.TypeCode, s1)

			// Altitude will be sent by dump1090 for ES ADS-B/TIS-B (DF=17 and DF=18)
			// and Mode S messages (DF=0, DF = 4, and DF = 20).

			ti.AltIsGNSS = newTi.AltIsGNSS

			if newTi.Alt != nil {
				ti.Alt = int32(*newTi.Alt)
				ti.Last_alt = stratuxClock.Time
			}

			if newTi.GnssDiffFromBaroAlt != nil {
				ti.GnssDiffFromBaroAlt = int32(*newTi.GnssDiffFromBaroAlt) // we can estimate pressure altitude from GNSS height with this parameter!
				ti.Last_GnssDiff = stratuxClock.Time
				ti.Last_GnssDiffAlt = ti.Alt
			}

			// Position updates are provided only by ES messages (DF=17 and DF=18; multiple TCs)
			if newTi.Position_valid { // i.e. DF17 or DF18 message decoded successfully by dump1090
				valid_position := true
				var lat, lng float32

				if newTi.Lat != nil {
					lat = float32(*newTi.Lat)
				} else { // dump1090 send a valid message, but Stratux couldn't figure it out for some reason.
					valid_position = false
					//log.Printf("Missing latitude in DF=17/18 airborne position message\n")
				}

				if newTi.Lng != nil {
					lng = float32(*newTi.Lng)
				} else { //
					valid_position = false
					//log.Printf("Missing longitude in DF=17 airborne position message\n")
				}

				if valid_position {
					ti.Lat = lat
					ti.Lng = lng
					if isGPSValid() {
						ti.Distance, ti.Bearing = distance(float64(mySituation.GPSLatitude), float64(mySituation.GPSLongitude), float64(ti.Lat), float64(ti.Lng))
						ti.BearingDist_valid = true
					}
					ti.Position_valid = true
					ti.ExtrapolatedPosition = false
					ti.Last_seen = stratuxClock.Time // only update "last seen" data on position updates
				}
			}

			if newTi.Speed_valid { // i.e. DF17 or DF18, TC 19 message decoded successfully by dump1090
				valid_speed := true
				var speed, track uint16

				if newTi.Track != nil {
					track = uint16(*newTi.Track)
				} else { // dump1090 send a valid message, but Stratux couldn't figure it out for some reason.
					valid_speed = false
					//log.Printf("Missing track in DF=17/18 TC19 airborne velocity message\n")
				}

				if newTi.Speed != nil {
					speed = uint16(*newTi.Speed)
				} else { //
					valid_speed = false
					//log.Printf("Missing speed in DF=17/18 TC19 airborne velocity message\n")
				}

				if newTi.Vvel != nil {
					ti.Vvel = int16(*newTi.Vvel)
				} else { // we'll still make the message without a valid vertical speed.
					//log.Printf("Missing vertical speed in DF=17/18 TC19 airborne velocity message\n")
				}

				if valid_speed {
					ti.Track = track
					ti.Speed = speed
					ti.Speed_valid = true
					ti.Last_speed = stratuxClock.Time // only update "last seen" data on position updates
				}
			} else if ((newTi.DF == 17) || (newTi.DF == 18)) && (newTi.TypeCode == 19) { // invalid speed on velocity message only
				ti.Speed_valid = false
			}

			// Determine NIC (navigation integrity category) from type code and subtype code
			if ((newTi.DF == 17) || (newTi.DF == 18)) && (newTi.TypeCode >= 5 && newTi.TypeCode <= 22) && (newTi.TypeCode != 19) {
				nic := 0 // default for unknown or missing NIC
				switch newTi.TypeCode {
				case 0, 8, 18, 22:
					nic = 0
				case 17:
					nic = 1
				case 16:
					if newTi.SubtypeCode == 1 {
						nic = 3
					} else {
						nic = 2
					}
				case 15:
					nic = 4
				case 14:
					nic = 5
				case 13:
					nic = 6
				case 12:
					nic = 7
				case 11:
					if newTi.SubtypeCode == 1 {
						nic = 9
					} else {
						nic = 8
					}
				case 10, 21:
					nic = 10
				case 9, 20:
					nic = 11
				}
				ti.NIC = nic

				if (ti.NACp < 7) && (ti.NACp < ti.NIC) {
					ti.NACp = ti.NIC // initialize to NIC, since NIC is sent with every position report, and not all emitters report NACp.
				}
			}

			if newTi.NACp != nil {
				ti.NACp = *newTi.NACp
			}

			if newTi.Emitter_category != nil {
				ti.Emitter_category = uint8(*newTi.Emitter_category) // validate dump1090 on live traffic
			}

			if newTi.Squawk != nil {
				ti.Squawk = int(*newTi.Squawk) // only provided by Mode S messages, so we don't do this in parseUAT.
			}
			// Set the target type. DF=18 messages are sent by ground station, so we look at CA
			// (repurposed to Control Field in DF18) to determine if it's ADS-R or TIS-B.
			if newTi.DF == 17 {
				ti.TargetType = TARGET_TYPE_ADSB
				ti.Addr_type = 0
			} else if newTi.DF == 18 {
				if newTi.CA == 6 {
					ti.TargetType = TARGET_TYPE_ADSR
					ti.Addr_type = 2
				} else if newTi.CA == 2 { // 2 = TIS-B with ICAO address, 5 = TIS-B without ICAO address
					ti.TargetType = TARGET_TYPE_TISB
					ti.Addr_type = 2
				} else if newTi.CA == 5 {
					ti.TargetType = TARGET_TYPE_TISB
					ti.Addr_type = 3
				}
			}

			if newTi.OnGround != nil { // DF=11 messages don't report "on ground" status so we need to check for valid values.
				ti.OnGround = bool(*newTi.OnGround)
			}

			if (newTi.Tail != nil) && ((newTi.DF == 17) || (newTi.DF == 18)) { // DF=17 or DF=18, Type Code 1-4
				ti.Tail = *newTi.Tail
				ti.Tail = strings.Trim(ti.Tail, " ") // remove extraneous spaces
			}

			// This is a hack to show the source of the traffic on moving maps.

			if globalSettings.DisplayTrafficSource {
				type_code := " "
				switch ti.TargetType {
				case TARGET_TYPE_ADSB:
					type_code = "a"
				case TARGET_TYPE_ADSR:
					type_code = "r"
				case TARGET_TYPE_TISB:
					type_code = "t"
				}

				if len(ti.Tail) == 0 {
					ti.Tail = "e" + type_code
				} else if len(ti.Tail) < 7 && ti.Tail[0] != 'e' && ti.Tail[0] != 'u' {
					ti.Tail = "e" + type_code + ti.Tail
				} else if len(ti.Tail) == 7 && ti.Tail[0] != 'e' && ti.Tail[0] != 'u' {
					ti.Tail = "e" + type_code + ti.Tail[1:]
				} else if len(ti.Tail) > 1 { // bounds checking
					ti.Tail = "e" + type_code + ti.Tail[2:]

				}
			}

			if newTi.DF == 17 || newTi.DF == 18 {
				ti.Last_source = TRAFFIC_SOURCE_1090ES // only update traffic source on ADS-B messages. Prevents source on UAT ADS-B targets with Mode S transponders from "flickering" every time we get an altitude or DF11 update.
			}
			ti.Timestamp = newTi.Timestamp // only update "last seen" data on position updates

			/*
				s_out, err := json.Marshal(ti)
				if err != nil {
					log.Printf("Error generating output: %s\n", err.Error())
				} else {
					log.Printf("%X (DF%d) => %s\n", ti.Icao_addr, newTi.DF, string(s_out))
				}
			*/

			traffic[ti.Icao_addr] = ti // Update information on this ICAO code.
			registerTrafficUpdate(ti)
			seenTraffic[ti.Icao_addr] = true // Mark as seen.
			//log.Printf("%v\n",traffic)
			trafficMutex.Unlock()

		}
	}
}

/*
updateDemoTraffic creates / updates a simulated traffic target for demonstration / debugging
purpose. Target will circle clockwise around the current GPS position (if valid) or around
KOSH, once every five minutes.

Inputs are ICAO 24-bit hex code, tail number (8 chars max), relative altitude in feet,
groundspeed in knots, and bearing offset from 0 deg initial position.

Traffic on headings 150-240 (bearings 060-150) is intentionally suppressed from updating to allow
for testing of EFB and webUI response. Additionally, the "on ground" flag is set for headings 240-270,
and speed invalid flag is set for headings 135-150 to allow testing of response to those conditions.

*/
func updateDemoTraffic(icao uint32, tail string, relAlt float32, gs float64, offset int32) {
	var ti TrafficInfo

	// Retrieve previous information on this ICAO code.
	if val, ok := traffic[icao]; ok { // if we've already seen it, copy it in to do updates
		ti = val
		//log.Printf("Existing target %X imported for ES update\n", icao)
	} else {
		//log.Printf("New target %X created for ES update\n",newTi.Icao_addr)
		ti.Last_seen = stratuxClock.Time // need to initialize to current stratuxClock so it doesn't get cut before we have a chance to populate a position message
		ti.Icao_addr = icao
		ti.ExtrapolatedPosition = false
	}
	hdg := float64((int32(stratuxClock.Milliseconds/1000)+offset)%720) / 2
	// gs := float64(220) // knots
	radius := gs * 0.2 / (2 * math.Pi)
	x := radius * math.Cos(hdg*math.Pi/180.0)
	y := radius * math.Sin(hdg*math.Pi/180.0)
	// default traffic location is Oshkosh if GPS not detected
	lat := 43.99
	lng := -88.56
	if isGPSValid() {
		lat = float64(mySituation.GPSLatitude)
		lng = float64(mySituation.GPSLongitude)
	}
	traffRelLat := y / 60
	traffRelLng := -x / (60 * math.Cos(lat*math.Pi/180.0))

	ti.Icao_addr = icao
	ti.OnGround = false
	ti.Addr_type = uint8(icao % 4) // 0 == ADS-B; 1 == reserved; 2 == TIS-B with ICAO address; 3 == TIS-B without ICAO address; 6 == ADS-R
	if ti.Addr_type == 1 {         // reassign "reserved value" to ADS-R
		ti.Addr_type = 6
	}

	if ti.Addr_type == 0 {
		ti.TargetType = TARGET_TYPE_ADSB
	} else if ti.Addr_type == 3 {
		ti.TargetType = TARGET_TYPE_TISB
	} else if ti.Addr_type == 6 {
		ti.TargetType = TARGET_TYPE_ADSR
	} else if ti.Addr_type == 2 {
		ti.TargetType = TARGET_TYPE_TISB_S
		if (ti.NIC >= 7) && (ti.Emitter_category > 0) { // If NIC is sufficiently high and emitter type is transmitted, we'll assume it's ADS-R.
			ti.TargetType = TARGET_TYPE_ADSR
		}
	}

	ti.Emitter_category = 1
	ti.Lat = float32(lat + traffRelLat)
	ti.Lng = float32(lng + traffRelLng)

	ti.Distance, ti.Bearing = distance(float64(lat), float64(lng), float64(ti.Lat), float64(ti.Lng))
	ti.BearingDist_valid = true

	ti.Position_valid = true
	ti.ExtrapolatedPosition = false
	ti.Alt = int32(mySituation.GPSAltitudeMSL + relAlt)
	ti.Track = uint16(hdg)
	ti.Speed = uint16(gs)
	if hdg >= 240 && hdg < 270 {
		ti.OnGround = true
	}
	if hdg > 135 && hdg < 150 {
		ti.Speed_valid = false
	} else {
		ti.Speed_valid = true
	}
	ti.Vvel = 0
	ti.Tail = tail // "DEMO1234"
	ti.Timestamp = time.Now()
	ti.Last_seen = stratuxClock.Time
	ti.Last_alt = stratuxClock.Time
	ti.Last_speed = stratuxClock.Time
	ti.NACp = 8
	ti.NIC = 8

	//ti.Age = math.Floor(ti.Age) + hdg / 1000
	ti.Last_source = 1
	if icao%5 == 1 { // make some of the traffic look like it came from UAT
		ti.Last_source = 2
	}

	if hdg < 150 || hdg > 240 {
		// now insert this into the traffic map...
		trafficMutex.Lock()
		defer trafficMutex.Unlock()
		traffic[ti.Icao_addr] = ti
		registerTrafficUpdate(ti)
		seenTraffic[ti.Icao_addr] = true
	}
}

/*
	icao2reg() : Converts 24-bit Mode S addresses to N-numbers and C-numbers.

			Input: uint32 representing the Mode S address. Valid range for
				translation is 0xA00001 - 0xADF7C7, inclusive.

				Values outside the range A000001-AFFFFFF or C00001-C3FFFF
				are flagged as foreign.

				Values between ADF7C8 - AFFFFF are allocated to the United States,
				but are not used for aicraft on the civil registry. These could be
				military, other public aircraft, or future use.

				Values between C0CDF9 - C3FFFF are allocated to Canada,
				but are not used for aicraft on the civil registry. These could be
				military, other public aircraft, or future use.

				Values between 7C0000 - 7FFFFF are allocated to Australia.


			Output:
				string: String containing the decoded tail number (if decoding succeeded),
					"NON-NA" (for non-US / non Canada allocation), and "US-MIL" or "CA-MIL" for non-civil US / Canada allocation.

				bool: True if the Mode S address successfully translated to an
					N number. False for all other conditions.
*/

func icao2reg(icao_addr uint32) (string, bool) {
	// Initialize local variables
	base34alphabet := string("ABCDEFGHJKLMNPQRSTUVWXYZ0123456789")
	nationalOffset := uint32(0xA00001) // default is US
	tail := ""
	nation := ""

	// Determine nationality
	if (icao_addr >= 0xA00001) && (icao_addr <= 0xAFFFFF) {
		nation = "US"
	} else if (icao_addr >= 0xC00001) && (icao_addr <= 0xC3FFFF) {
		nation = "CA"
	} else if (icao_addr >= 0x7C0000) && (icao_addr <= 0x7FFFFF) {
		nation = "AU"
	} else {
		//TODO: future national decoding.
		return "OTHER", false
	}

	if nation == "CA" { // Canada decoding
		// First, discard addresses that are not assigned to aircraft on the civil registry
		if icao_addr > 0xC0CDF8 {
			//fmt.Printf("%X is a Canada aircraft, but not a CF-, CG-, or CI- registration.\n", icao_addr)
			return "CA-MIL", false
		}

		nationalOffset := uint32(0xC00001)
		serial := int32(icao_addr - nationalOffset)

		// Fifth letter
		e := serial % 26

		// Fourth letter
		d := (serial / 26) % 26

		// Third letter
		c := (serial / 676) % 26 // 676 == 26*26

		// Second letter
		b := (serial / 17576) % 26 // 17576 == 26*26*26

		b_str := "FGI"

		//fmt.Printf("B = %d, C = %d, D = %d, E = %d\n",b,c,d,e)
		tail = fmt.Sprintf("C-%c%c%c%c", b_str[b], c+65, d+65, e+65)
	}

	if nation == "AU" { // Australia decoding

		nationalOffset := uint32(0x7C0000)
		offset := (icao_addr - nationalOffset)
		i1 := offset / 1296
		offset2 := offset % 1296
		i2 := offset2 / 36
		offset3 := offset2 % 36
		i3 := offset3

		var a_char, b_char, c_char string

		a_char = fmt.Sprintf("%c", i1+65)
		b_char = fmt.Sprintf("%c", i2+65)
		c_char = fmt.Sprintf("%c", i3+65)

		if i1 < 0 || i1 > 25 || i2 < 0 || i2 > 25 || i3 < 0 || i3 > 25 {
			return "OTHER", false
		}

		tail = "VH-" + a_char + b_char + c_char
	}

	if nation == "US" { // FAA decoding
		// First, discard addresses that are not assigned to aircraft on the civil registry
		if icao_addr > 0xADF7C7 {
			//fmt.Printf("%X is a US aircraft, but not on the civil registry.\n", icao_addr)
			return "US-MIL", false
		}

		serial := int32(icao_addr - nationalOffset)
		// First digit
		a := (serial / 101711) + 1

		// Second digit
		a_remainder := serial % 101711
		b := ((a_remainder + 9510) / 10111) - 1

		// Third digit
		b_remainder := (a_remainder + 9510) % 10111
		c := ((b_remainder + 350) / 951) - 1

		// This next bit is more convoluted. First, figure out if we're using the "short" method of
		// decoding the last two digits (two letters, one letter and one blank, or two blanks).
		// This will be the case if digit "B" or "C" are calculated as negative, or if c_remainder
		// is less than 601.

		c_remainder := (b_remainder + 350) % 951
		var d, e int32

		if (b >= 0) && (c >= 0) && (c_remainder > 600) { // alphanumeric decoding method
			d = 24 + (c_remainder-601)/35
			e = (c_remainder - 601) % 35

		} else { // two-letter decoding method
			if (b < 0) || (c < 0) {
				c_remainder -= 350 // otherwise "  " == 350, "A " == 351, "AA" == 352, etc.
			}

			d = (c_remainder - 1) / 25
			e = (c_remainder - 1) % 25

			if e < 0 {
				d -= 1
				e += 25
			}
		}

		a_char := fmt.Sprintf("%d", a)
		var b_char, c_char, d_char, e_char string

		if b >= 0 {
			b_char = fmt.Sprintf("%d", b)
		}

		if b >= 0 && c >= 0 {
			c_char = fmt.Sprintf("%d", c)
		}

		if d > -1 {
			d_char = string(base34alphabet[d])
			if e > 0 {
				e_char = string(base34alphabet[e-1])
			}
		}

		tail = "N" + a_char + b_char + c_char + d_char + e_char

	}

	return tail, true
}

func initTraffic() {
	traffic = make(map[uint32]TrafficInfo)
	seenTraffic = make(map[uint32]bool)
	trafficMutex = &sync.Mutex{}
	go esListen()
}
