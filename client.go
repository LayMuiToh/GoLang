package main

import (
    "bytes"
    "encoding/json"
    "flag"
    "fmt"
    "io"
    "io/ioutil"
    "log"
    "net/url"
    "os"
    "os/signal"
    "path/filepath"
    "strconv"
    "strings"
    "sync"
    "time"
    "encoding/binary"

    "github.com/gorilla/websocket"
)

type wavearr_t []string

func (i *wavearr_t) String() string {
    return fmt.Sprintf("%s", *i)
}

func (i *wavearr_t) Set(value string) error {
    *i = append(*i, value)
    return nil
}

var multiwav wavearr_t

func showEnv() {
    fmt.Printf(`
    Known Environments
    ------------------

    CAT    dev    tap01.dev.exm-platform.com:9350
    CAT    prd    cat-ap.prod.exm-platform.com:9350
    KB4    dev    kb4-dev.exm-platform.com:9350
    KB4    ecs    websocket-server.aws-kb4-dev.exm-platform.com:443
    KB4    lt     52.221.181.65:9350
    BLD    dev    127.0.0.1:9350


`)
}

var fldr = flag.String("wave-folder", "", "Folder to glob for wave audio")
var addr = flag.String("endpoint", "127.0.0.1:9350", "Websocket Server Address")
var ssl = flag.Bool("ssl", true, "Use an SSL connection")
var iters = flag.Int("iterations", 1, "Number of iterations for the test")
var parallel = flag.Int("parallel", 1, "Number of parallel clients to spawn")
var env = flag.Bool("env", false, "Help on known environments (for endpoint)")

type chatAckType struct {
    MessageType string `json:"messageType"`
    Sequence    int    `json:"sequence"`
}

type AudioControl struct {
    messageType string
}

type AsrReply struct {
    Result map[string]string `json:"result"`
}

func croak(msg string) {
    log.Printf("\x1b[31;1mERROR\x1b[0m: %s", msg)
}

func check(e error) {
    if e != nil {
        log.Printf("\x1b[31;1mFAILURE\x1b[0m: %s", e)
        panic(e)
    }
}

type WaveHeader struct {
    ChunkID [4]byte
    ChunkSize int32
    Format  [4]byte
}

type WaveSubChunk struct {
    ChunkID [4]byte
    ChunkSize int32
}

func Split(r rune) bool {
    return r == ':' || r == ';'
}

func test_audio(wss *websocket.Conn, wavdata []byte, count int, expect string) int {

    // First write our audio-start message
    m := map[string]string{"messageType": "audio-start"}
    start, err := json.Marshal(m)
    log.Printf("Sending message %s", start)
    err = wss.WriteMessage(websocket.TextMessage, start)

    // Check if we're reading a wave file
    hdr := WaveHeader{}
    rdr := bytes.NewReader(wavdata)
    err = binary.Read(rdr, binary.LittleEndian, &hdr)
    header_id := string(hdr.ChunkID[:4])
    format := string(hdr.Format[:4])
    offset := 12 // RIFF header

    if format == "WAVE" {
        log.Printf("Detected WAV audio format ...")
        chunk := WaveSubChunk{}
        // Seek out data
        for header_id != "data" {
            offset += int(chunk.ChunkSize)
            if header_id == "txts" {
                // Lets see whats in metadata
                metadata := make([]byte, chunk.ChunkSize)
                err = binary.Read(rdr, binary.LittleEndian, &metadata)
                log.Printf("Metadata: \n%s", string(metadata))

            } else {
                rdr.Seek(int64(chunk.ChunkSize), io.SeekCurrent)
            }
            err = binary.Read(rdr, binary.LittleEndian, &chunk)
            if err != nil {
                break
            }
            header_id = string(chunk.ChunkID[:4])
            log.Printf("  -> %s", header_id)
            offset += 8 // WaveSubChunk
        }
    }

    // Pump the audio data in
    log.Printf("Sending audio data ...")
    err = wss.WriteMessage(websocket.BinaryMessage, wavdata[offset:])

    // Indicate a stop audio
    m = map[string]string{"messageType": "audio-end"}
    stop, err := json.Marshal(m)
    log.Printf("Sending message %s", stop)
    err = wss.WriteMessage(websocket.TextMessage, stop)

    // Read and parse the response
    _, msg, err := wss.ReadMessage()
    check(err)
    if string(msg[:4]) == "DXTL" {

        // Chew up everything until after the newline
        if i := strings.IndexByte(string(msg), '\n'); i >= 0 {
            hdrstamp := msg[:i]
            msg = msg[i:]

            a := strings.FieldsFunc(string(hdrstamp), Split)
            _, _, seq := a[0], a[1], a[2]
            seqi, _ := strconv.Atoi(seq)
            if count%100 == 0 {
                log.Printf("[Skip Ack] %d", seqi)
                // Wait for a bit and we should get the ack
                time.Sleep(10 * time.Millisecond)
                _, msg, err := wss.ReadMessage()
                check(err)
                log.Printf("[Received|Replay] %s", msg)

            } else {
                ack := chatAckType{
                    MessageType: "chat-ack",
                    Sequence:    seqi,
                }
                ack_m, err := json.Marshal(ack)
                log.Printf("[Sent|Ack] %s", ack_m)
                err = wss.WriteMessage(websocket.TextMessage, ack_m)
                check(err)
            }
        }
    }

    reply := AsrReply{}
    err = json.Unmarshal(msg, &reply)
    response_text := reply.Result["r0"]

    success := 0
    if strings.ToLower(response_text) == strings.ToLower(expect) {
        log.Printf("\x1b[34;1mSUCCESS\x1b[0m: %s", response_text)
        success = 1
    } else {
        log.Printf("\x1b[31;1mFAILURE\x1b[0m: %s", msg)
    }
    return success
}

func main() {
    flag.Parse()
    log.SetFlags(0)
    var destination string

    if *env {
        showEnv()
        os.Exit(-1)
    }

    proto := "ws"
    if *ssl {
        proto = "wss"
    }

    if *fldr != "" {
        multiwav, _ = filepath.Glob(*fldr + "/*.wav")
        // FIXME: Append arrays rather than either or
        if len(multiwav) == 0 {
            multiwav, _ = filepath.Glob(*fldr + "/*.raw")
        }
    }

    if len(multiwav) == 0 {
        croak("Folder has no audio files (.[raw|wav])")
        os.Exit(-1)
    }

    if *addr == "" {
        croak("You must specify a target endpoint")
        os.Exit(-1)
    }

    interrupt := make(chan os.Signal, 1)
    signal.Notify(interrupt, os.Interrupt)
    go func() {
        for sig := range interrupt {
            log.Printf("\n[%s] Exiting ...", sig)
            os.Exit(-1)
        }
    }()

    destination = *addr

    u := url.URL{Scheme: proto, Host: destination}

    var wg sync.WaitGroup
    wg.Add(*parallel)
    replies := make(chan int)

    for i := 0; i < *parallel; i++ {
        go func() {
            defer wg.Done()
            log.Printf("[%d] Connecting to %s", i, u.String())
            wss, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
            check(err)
            defer wss.Close()
            for i := 0; i < len(multiwav); i++ {
                log.Printf("[ Processing WAV %s ] ", multiwav[i])
                inputfile := multiwav[i]

                if _, err := os.Stat(inputfile); os.IsNotExist(err) {
                    log.Printf("File %s not found", inputfile)
                } else {
                    // Extract the expected text
                    expected := filepath.Base(inputfile)
                    expected = strings.TrimSuffix(expected, filepath.Ext(expected))
                    expected = strings.Replace(expected, "_", " ", -1)
                    log.Printf("Expected text [%s]", expected)

                    // Read in the file itself
                    log.Printf("Reading audio source data %s", inputfile)
                    wavdata, err := ioutil.ReadFile(inputfile)
                    check(err)

                    for i := 1; i <= *iters; i++ {
                        status := test_audio(wss, wavdata, i, expected)
                        replies <- status
                    }
                }
            }
        }()
    }

    successes := 0
    go func() {
        for response := range replies {
            successes += response
        }
    }()

    wg.Wait()

    fmt.Printf("Successfully matched %d replies out of %d requests",
        successes, (*iters)*(*parallel))

}
