// Copyright (c) 2015-2024 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"reflect"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/fasthttp/websocket"
	"github.com/minio/hperf/shared"
)

var (
	responseDPS    = make([]shared.DP, 0)
	responseERR    = make([]shared.TError, 0)
	responseLock   = sync.Mutex{}
	websockets     []*wsClient
	hostsDoingWork atomic.Int32
)

type wsClient struct {
	ID   int
	Host string
	Con  *websocket.Conn
}

func (c *wsClient) SendError(e error) error {
	if e == nil {
		return nil
	}
	msg := new(shared.WebsocketSignal)
	msg.SType = shared.Err
	msg.Error = e.Error()
	return c.Con.WriteJSON(msg)
}

func (c *wsClient) Close() (err error) {
	return c.Remove()
}

func (c *wsClient) Remove() (err error) {
	err = c.Con.Close()
	websockets[c.ID] = nil
	return
}

func itterateWebsockets(action func(c *wsClient)) {
	for i := 0; i < len(websockets); i++ {
		if websockets[i] == nil {
			continue
		}
		action(websockets[i])
	}
}

func (c *wsClient) NewSignal(signal shared.SignalType, conf *shared.Config) *shared.WebsocketSignal {
	msg := new(shared.WebsocketSignal)
	msg.SType = signal
	msg.Config = conf
	return msg
}

func (c *wsClient) Ping() (err error) {
	msg := new(shared.WebsocketSignal)
	msg.SType = shared.Ping
	err = c.Con.WriteJSON(msg)
	return
}

var (
	testList = make(map[string]shared.TestInfo)
	testLock = sync.Mutex{}
)

func initializeClient(ctx context.Context, c *shared.Config) (err error) {
	websockets = make([]*wsClient, len(c.Hosts))

	clientID := 0
	done := make(chan struct{}, len(c.Hosts))
	for _, host := range c.Hosts {
		go handleWSConnection(ctx, c, host, clientID, done)
		clientID++
	}

	doneCount := 0
	timeout := time.NewTicker(time.Second * 10)

	for {
		select {
		case <-done:
			doneCount++
			hostsDoingWork.Add(1)
			if doneCount == len(c.Hosts) {
				return
			}
		case <-ctx.Done():
			return errors.New("Context canceled")
		case <-timeout.C:
			return errors.New("Timeout when connecting to hosts")
		}
	}
}

func handleWSConnection(ctx context.Context, c *shared.Config, host string, id int, done chan struct{}) {
	var err error
	defer func() {
		r := recover()
		if r != nil {
			fmt.Println(r, string(debug.Stack()))
		}
		if ctx.Err() != nil {
			hostsDoingWork.Add(-1)
			return
		}
		if c.RestartOnError && err != nil {
			time.Sleep(500 * time.Millisecond)
			go handleWSConnection(ctx, c, host, id, done)
		} else {
			hostsDoingWork.Add(-1)
		}
	}()

	socket := websockets[id]
	if socket == nil {
		websockets[id] = new(wsClient)
		socket = websockets[id]
		socket.ID = id
	}

	socket.Host = host

	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: time.Second * c.DialTimeout,
		ReadBufferSize:   1000000,
		WriteBufferSize:  1000000,
	}

	shared.DEBUG(WarningStyle.Render("Connecting to ", host, ":", c.Port))

	connectString := "wss://" + host + ":" + c.Port + "/ws/" + host
	if c.Insecure {
		connectString = "ws://" + host + ":" + c.Port + "/ws/" + host
	}

	con, _, dialErr := dialer.DialContext(
		ctx,
		connectString,
		nil)
	if dialErr != nil {
		PrintError(dialErr)
		err = dialErr
		return
	}
	socket.Con = con

	msg := new(shared.WebsocketSignal)
	err = con.ReadJSON(&msg)
	if err != nil {
		err = fmt.Errorf("Unable to read message from server on first connect %s", err)
		PrintError(err)
		return
	}
	if msg.Code != shared.OK {
		err = fmt.Errorf("Received %d from server on connect", msg.Code)
		PrintError(err)
		return
	}
	shared.DEBUG(SuccessStyle.Render("Connected to ", host, ":", c.Port))

	done <- struct{}{}
	for {
		signal := new(shared.WebsocketSignal)
		err = con.ReadJSON(&signal)
		if err != nil {
			PrintError(err)
			return
		}
		if shared.DebugEnabled {
			fmt.Printf("WebsocketSignal: %+v\n", signal)
		}
		switch signal.SType {
		case shared.Stats:
			go collectDataPointv2(signal.DataPoint)
		case shared.ListTests:
			go parseTestList(signal.TestList)
		case shared.GetTest:
			go receiveJSONDataPoint(signal.Data, c)
		case shared.Err:
			go PrintErrorString(signal.Error)
		case shared.Done:
			shared.DEBUG(SuccessStyle.Render("Host Finished: ", con.RemoteAddr().String()))
			return
		}
	}
}

func PrintTError(err shared.TError) {
	fmt.Println(ErrorStyle.Render(err.Created.Format(time.RFC3339), " - ", err.Error))
}

func PrintErrorString(err string) {
	fmt.Println(ErrorStyle.Render(err))
}

func PrintError(err error) {
	if err == nil {
		return
	}
	fmt.Println(ErrorStyle.Render("ERROR: ", err.Error()))
}

func receiveJSONDataPoint(data []byte, _ *shared.Config) {
	responseLock.Lock()
	defer responseLock.Unlock()

	if bytes.HasPrefix(data, shared.ErrorPoint.String()) {
		dp := new(shared.TError)
		err := json.Unmarshal(data[1:], &dp)
		if err != nil {
			PrintError(err)
			return
		}
		responseERR = append(responseERR, *dp)
	} else if bytes.HasPrefix(data, shared.DataPoint.String()) {
		dp := new(shared.DP)
		err := json.Unmarshal(data[1:], &dp)
		if err != nil {
			PrintError(err)
			return
		}
		responseDPS = append(responseDPS, *dp)
	} else {
		PrintError(fmt.Errorf("Uknown data point: %s", data))
	}
}

func keepAliveLoop(ctx context.Context, c *shared.Config, tickerfunc func() (shouldExit bool)) error {
	start := time.Now()
	for ctx.Err() == nil {
		time.Sleep(1 * time.Second)
		if time.Since(start).Seconds() > float64(c.Duration)+20 {
			return errors.New("Total duration reached 20 seconds past the configured duration")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if tickerfunc != nil && tickerfunc() {
			break
		}

		if hostsDoingWork.Load() <= 0 {
			return ctx.Err()
		}

	}
	return ctx.Err()
}

func Listen(ctx context.Context, c shared.Config) (err error) {
	cancelContext, cancel := context.WithCancel(ctx)
	defer cancel()
	err = initializeClient(cancelContext, &c)
	if err != nil {
		return
	}

	itterateWebsockets(func(ws *wsClient) {
		err = ws.Con.WriteJSON(ws.NewSignal(shared.ListenTest, &c))
		if err != nil {
			return
		}
	})

	return keepAliveLoop(ctx, &c, nil)
}

func Stop(ctx context.Context, c shared.Config) (err error) {
	cancelContext, cancel := context.WithCancel(ctx)
	defer cancel()
	err = initializeClient(cancelContext, &c)
	if err != nil {
		return
	}

	itterateWebsockets(func(ws *wsClient) {
		err = ws.Con.WriteJSON(ws.NewSignal(shared.StopAllTests, &c))
		if err != nil {
			return
		}
	})

	return keepAliveLoop(ctx, &c, nil)
}

func RunTest(ctx context.Context, c shared.Config) (err error) {
	cancelContext, cancel := context.WithCancel(ctx)
	defer cancel()
	err = initializeClient(cancelContext, &c)
	if err != nil {
		return
	}

	itterateWebsockets(func(ws *wsClient) {
		err = ws.Con.WriteJSON(ws.NewSignal(shared.RunTest, &c))
		if err != nil {
			return
		}
	})

	printCount := 0

	printOnTick := func() bool {
		if len(responseDPS) == 0 {
			return false
		}
		printCount++

		to := new(shared.TestOutput)
		to.ErrCount = len(responseERR)
		to.TXL = math.MaxInt64
		to.RMSL = math.MaxInt64
		to.TTFBL = math.MaxInt64
		to.ML = responseDPS[0].MemoryUsedPercent
		to.CL = responseDPS[0].CPUUsedPercent
		tt := responseDPS[0].Type

		for i := range responseDPS {
			to.TXC += responseDPS[i].TXCount
			to.TXT += responseDPS[i].TXTotal
			to.DP = responseDPS[i].DroppedPackets

			if to.TXL > responseDPS[i].TX {
				to.TXL = responseDPS[i].TX
			}
			if to.RMSL > responseDPS[i].RMSL {
				to.RMSL = responseDPS[i].RMSL
			}
			if to.TTFBL > responseDPS[i].TTFBL {
				to.TTFBL = responseDPS[i].TTFBL
			}
			if to.ML > responseDPS[i].MemoryUsedPercent {
				to.ML = responseDPS[i].MemoryUsedPercent
			}
			if to.CL > responseDPS[i].CPUUsedPercent {
				to.CL = responseDPS[i].CPUUsedPercent
			}

			if to.TXH < responseDPS[i].TX {
				to.TXH = responseDPS[i].TX
			}
			if to.RMSH < responseDPS[i].RMSH {
				to.RMSH = responseDPS[i].RMSH
			}
			if to.TTFBH < responseDPS[i].TTFBH {
				to.TTFBH = responseDPS[i].TTFBH
			}
			if to.MH < responseDPS[i].MemoryUsedPercent {
				to.MH = responseDPS[i].MemoryUsedPercent
			}
			if to.CH < responseDPS[i].CPUUsedPercent {
				to.CH = responseDPS[i].CPUUsedPercent
			}
		}

		if !c.Micro {
			to.TTFBH = to.TTFBH / 1000
			to.TTFBL = to.TTFBL / 1000
			to.RMSH = to.RMSH / 1000
			to.RMSL = to.RMSL / 1000
		}

		for i := range responseERR {
			PrintErrorString(responseERR[i].Error)
		}

		if printCount%10 == 1 {
			printRealTimeHeaders(tt)
		}
		printRealTimeRow(BaseStyle, to, tt)

		return false
	}

	return keepAliveLoop(ctx, &c, printOnTick)
}

func ListTests(ctx context.Context, c shared.Config) (err error) {
	cancelContext, cancel := context.WithCancel(ctx)
	defer cancel()
	err = initializeClient(cancelContext, &c)
	if err != nil {
		return
	}

	itterateWebsockets(func(ws *wsClient) {
		err = ws.Con.WriteJSON(ws.NewSignal(shared.ListTests, &c))
		if err != nil {
			return
		}
	})

	err = keepAliveLoop(ctx, &c, nil)
	if err != nil {
		return
	}

	printHeader(ListHeaders)
	tableStyle := lipgloss.NewStyle()

	keys := []string{}
	for id := range testList {
		keys = append(keys, id)
	}

	slices.SortFunc(keys, func(a string, b string) int {
		if testList[a].Time.Before(testList[b].Time) {
			return 1
		} else {
			return -1
		}
	})

	for i := range keys {
		PrintColumns(
			tableStyle,
			column{strconv.Itoa(i), headerSlice[IntNumber].width},
			column{keys[i], headerSlice[ID].width},
			column{testList[keys[i]].Time.Format("02/01/2006 3:04 PM"), headerSlice[ID].width},
		)
	}

	return err
}

func DeleteTests(ctx context.Context, c shared.Config) (err error) {
	cancelContext, cancel := context.WithCancel(ctx)
	defer cancel()
	err = initializeClient(cancelContext, &c)
	if err != nil {
		return
	}

	itterateWebsockets(func(ws *wsClient) {
		err = ws.Con.WriteJSON(ws.NewSignal(shared.DeleteTests, &c))
		if err != nil {
			return
		}
	})

	return keepAliveLoop(ctx, &c, nil)
}

func parseTestList(list []shared.TestInfo) {
	testLock.Lock()
	defer testLock.Unlock()

	for i := range list {
		_, ok := testList[list[i].ID]
		if !ok {
			testList[list[i].ID] = list[i]
		}
	}
}

func DownloadTest(ctx context.Context, c shared.Config) (err error) {
	cancelContext, cancel := context.WithCancel(ctx)
	defer cancel()
	err = initializeClient(cancelContext, &c)
	if err != nil {
		return
	}

	itterateWebsockets(func(ws *wsClient) {
		err = ws.Con.WriteJSON(ws.NewSignal(shared.GetTest, &c))
		if err != nil {
			fmt.Println(err)
			return
		}
	})

	_ = keepAliveLoop(ctx, &c, nil)

	slices.SortFunc(responseERR, func(a shared.TError, b shared.TError) int {
		if a.Created.Before(b.Created) {
			return -1
		} else {
			return 1
		}
	})

	slices.SortFunc(responseDPS, func(a shared.DP, b shared.DP) int {
		if a.Created.Before(b.Created) {
			return -1
		} else {
			return 1
		}
	})

	f, err := os.Create(c.File)
	if err != nil {
		return err
	}
	defer f.Close()
	for i := range responseDPS {
		_, err := shared.WriteStructAndNewLineToFile(f, shared.DataPoint, responseDPS[i])
		if err != nil {
			return err
		}
	}
	for i := range responseERR {
		_, err := shared.WriteStructAndNewLineToFile(f, shared.ErrorPoint, responseERR[i])
		if err != nil {
			return err
		}
	}

	return nil
}

func AnalyzeBandwidthTest(ctx context.Context, c shared.Config) (err error) {
	_, cancel := context.WithCancel(ctx)
	defer cancel()

	if c.PrintAll {
		shared.INFO(" Printing all data points ..")
		fmt.Println("")

		printSliceOfDataPoints(responseDPS, c)

		if len(responseERR) > 0 {
			fmt.Println(" ____ ERRORS ____")
		}
		for i := range responseERR {
			PrintTError(responseERR[i])
		}
		if len(responseERR) > 0 {
			fmt.Println("")
		}
	}

	if len(responseDPS) == 0 {
		fmt.Println("No datapoints found")
		return
	}

	return nil
}

func AnalyzeLatencyTest(ctx context.Context, c shared.Config) (err error) {
	_, cancel := context.WithCancel(ctx)
	defer cancel()

	if c.PrintAll {
		shared.INFO(" Printing all data points ..")

		printSliceOfDataPoints(responseDPS, c)

		if len(responseERR) > 0 {
			fmt.Println(" ____ ERRORS ____")
		}
		for i := range responseERR {
			PrintTError(responseERR[i])
		}
		if len(responseERR) > 0 {
			fmt.Println("")
		}
	}
	if len(responseDPS) == 0 {
		fmt.Println("No datapoints found")
		return
	}

	shared.INFO(" Analyzing data ..")
	fmt.Println("")
	analyzeLatencyTest(responseDPS, c)

	return nil
}

func AnalyzeTest(ctx context.Context, c shared.Config) (err error) {
	_, cancel := context.WithCancel(ctx)
	defer cancel()

	f, err := os.Open(c.File)
	if err != nil {
		return err
	}
	defer f.Close()

	dps := make([]shared.DP, 0)
	errors := make([]shared.TError, 0)

	s := bufio.NewScanner(f)
	for s.Scan() {
		b := s.Bytes()
		if bytes.HasPrefix(b[1:], shared.ErrorPoint.String()) {
			dperr := new(shared.TError)
			err := json.Unmarshal(b, dperr)
			if err != nil {
				return err
			}
			errors = append(errors, *dperr)
		} else if bytes.HasPrefix(b, shared.DataPoint.String()) {
			dp := new(shared.DP)
			err := json.Unmarshal(b[1:], dp)
			if err != nil {
				return err
			}
			dps = append(dps, *dp)
		} else {
			shared.DEBUG(ErrorStyle.Render("Unknown data point encountered: ", string(b)))
		}
	}

	if c.HostFilter != "" {
		dps = shared.HostFilter(c.HostFilter, dps)
	}

	if c.PrintStats {
		printSliceOfDataPoints(dps, c)
	}

	if c.PrintErrors {
		if len(errors) > 0 {
			fmt.Println(" ____ ERRORS ____")
		}
		for i := range errors {
			PrintTError(errors[i])
		}
		if len(errors) > 0 {
			fmt.Println("")
		}
	}

	if len(dps) == 0 {
		fmt.Println("No datapoints found")
		return
	}

	switch dps[0].Type {
	case shared.RequestTest:
		analyzeLatencyTest(dps, c)
	case shared.StreamTest:
		fmt.Println("")
		fmt.Println("Detailed analysis for bandwidth testing is in development")
	}

	return nil
}

func analyzeLatencyTest(dps []shared.DP, c shared.Config) {
	shared.SortDataPoints(dps, c)

	dps10 := math.Ceil((float64(len(dps)) / 100) * 10)
	dps50 := math.Floor((float64(len(dps)) / 100) * 50)
	dps90 := math.Floor((float64(len(dps)) / 100) * 90)
	dps99 := math.Floor((float64(len(dps)) / 100) * 99)

	dps10s := make([]shared.DP, 0)
	dps50s := make([]shared.DP, 0)
	dps90s := make([]shared.DP, 0)
	dps99s := make([]shared.DP, 0)

	// count, sum, low, avg, high
	dps10stats := []int64{0, 0, math.MaxInt64, 0, 0}
	dps50stats := []int64{0, 0, math.MaxInt64, 0, 0}
	dps90stats := []int64{0, 0, math.MaxInt64, 0, 0}
	dps99stats := []int64{0, 0, math.MaxInt64, 0, 0}

	for i := range dps {
		if i >= int(dps10) {
			dps10s = append(dps10s, dps[i])
			shared.UpdatePSStats(dps10stats, dps[i], c)
		}
		if i >= int(dps50) {
			dps50s = append(dps50s, dps[i])
			shared.UpdatePSStats(dps50stats, dps[i], c)
		}
		if i >= int(dps90) {
			dps90s = append(dps90s, dps[i])
			shared.UpdatePSStats(dps90stats, dps[i], c)
		}
		if i >= int(dps99) {
			dps99s = append(dps99s, dps[i])
			shared.UpdatePSStats(dps99stats, dps[i], c)
		}
	}

	fmt.Println("")
	fmt.Println(" _____ P99 data points _____ ")
	fmt.Println("")
	printSliceOfDataPoints(dps99s, c)

	fmt.Println("")
	if c.Sort == "" {
		fmt.Println(" Sorting:", shared.SortDefault)
	} else {
		fmt.Println(" Sorting:", c.Sort)
	}
	if c.Micro {
		fmt.Println(" Time: Microseconds")
	} else {
		fmt.Println(" Time: Milliseconds")
	}
	fmt.Println("")
	PrintPercentiles(SuccessStyle, "P10", dps10stats, c)
	PrintPercentiles(WarningStyle, "P50", dps50stats, c)
	PrintPercentiles(ErrorStyle, "P90", dps90stats, c)
	PrintPercentiles(ErrorStyle, "P99", dps99stats, c)
}

func MakeCSV(ctx context.Context, c shared.Config) (err error) {
	byteValue, err := os.ReadFile(c.File)
	if err != nil {
		return err
	}

	file, err := os.Create(c.File + ".csv")
	if err != nil {
		return err
	}
	defer file.Close()

	fb := bytes.NewBuffer(byteValue)
	scanner := bufio.NewScanner(fb)

	writer := csv.NewWriter(file)
	defer writer.Flush()
	if err := writer.Write(getStructFields(new(shared.DP))); err != nil {
		return err
	}

	for scanner.Scan() {
		b := scanner.Bytes()
		if bytes.HasPrefix(b, shared.DataPoint.String()) {
			dp := new(shared.DP)
			err = json.Unmarshal(b[1:], dp)
			if err != nil {
				return err
			}

			if err := writer.Write(dpToSlice(dp)); err != nil {
				return err
			}
		}
	}

	return nil
}

// Function to get field names of the struct
func getStructFields(s interface{}) []string {
	t := reflect.TypeOf(s).Elem()
	fields := make([]string, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		fields[i] = t.Field(i).Tag.Get("json")
		if fields[i] == "" {
			fields[i] = t.Field(i).Name
		}
	}
	return fields
}

func dpToSlice(dp *shared.DP) (data []string) {
	v := reflect.ValueOf(dp).Elem()
	data = make([]string, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		data[i] = fmt.Sprintf("%v", v.Field(i).Interface())
	}
	return
}

func transformDataPointsToMilliseconds(dps []shared.DP) (clone []shared.DP) {
	clone = make([]shared.DP, len(dps))
	copy(clone, dps)
	for i := range clone {
		clone[i].TTFBH = clone[i].TTFBH / 1000
		clone[i].TTFBL = clone[i].TTFBL / 1000
		clone[i].RMSH = clone[i].RMSH / 1000
		clone[i].RMSL = clone[i].RMSL / 1000
	}
	return
}

func printSliceOfDataPoints(dps []shared.DP, c shared.Config) {
	var data []shared.DP
	if !c.Micro {
		data = transformDataPointsToMilliseconds(dps)
	} else {
		data = dps
	}

	for i := range data {
		if i%20 == 0 {
			printDataPointHeaders(data[0].Type)
		}
		dp := data[i]
		printTableRow(BaseStyle, &dp, dp.Type)
	}
}
