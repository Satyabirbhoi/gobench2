package main

import (
    "bufio"
    "bytes"
    "encoding/json"
    "flag"
    "fmt"
    "io"
    "io/ioutil"
    "log"
    "math/rand"
    "net"
    "os"
    "os/signal"
    "path/filepath"
    "runtime"
    "sync"
    "sync/atomic"
    "time"

    "github.com/valyala/fasthttp"
)

var (
    requests         int64
    period           int64
    clients          int
    url              string
    urlsFilePath     string
    keepAlive        bool
    postDataFilePath string
    writeTimeout     int
    readTimeout      int
    authHeader       string
    contentType      string
    apiUserName      string
    responseFileDir  string
)

// ResponseData is a struct to store the response data for each request.
type ResponseData struct {
    RequestNumber int64  `json:"requestNumber"`
    StatusCode    int    `json:"statusCode"`
    ResponseData  []byte `json:"responseData"`
    TXN_ID        int32  `json:"txnID"`
}

// Configuration represents the configuration for load testing.
type Configuration struct {
    urls            []string
    method          string
    postData        []byte
    requests        int64
    period          int64
    keepAlive       bool
    authHeader      string
    contentType     string
    apiUserName     string
    responseFileDir string
    myClient        fasthttp.Client
    responseFile    *os.File // Add a response file handle
}

type Result struct {
    Requests      int64
    Success       int64
    NetworkFailed int64
    BadFailed     int64
}

var readThroughput int64
var writeThroughput int64

type MyConn struct {
    net.Conn
}

func (this *MyConn) Read(b []byte) (n int, err error) {
    len, err := this.Conn.Read(b)

    if err == nil {
        atomic.AddInt64(&readThroughput, int64(len))
    }

    return len, err
}

func (this *MyConn) Write(b []byte) (n int, err error) {
    len, err := this.Conn.Write(b)

    if err == nil {
        atomic.AddInt64(&writeThroughput, int64(len))
    }

    return len, err
}

func init() {
    flag.Int64Var(&requests, "r", -1, "Number of requests per client")
    flag.IntVar(&clients, "c", 100, "Number of concurrent clients")
    flag.StringVar(&url, "u", "", "URL")
    flag.StringVar(&urlsFilePath, "f", "", "URL's file path (line seperated)")
    flag.BoolVar(&keepAlive, "k", true, "Do HTTP keep-alive")
    flag.StringVar(&postDataFilePath, "d", "", "HTTP POST data file path")
    flag.Int64Var(&period, "t", -1, "Period of time (in seconds)")
    flag.IntVar(&writeTimeout, "tw", 5000, "Write timeout (in milliseconds)")
    flag.IntVar(&readTimeout, "tr", 5000, "Read timeout (in milliseconds)")
    flag.StringVar(&authHeader, "auth", "", "Authorization header")
    flag.StringVar(&contentType, "ct", "", "Content type")
    flag.StringVar(&apiUserName, "user", "", "API User Name")
    flag.StringVar(&responseFileDir, "rsp", "", "Directory path to store response json files")
}

func printResults(results map[int]*Result, startTime time.Time) {
    var requests int64
    var success int64
    var networkFailed int64
    var badFailed int64

    for _, result := range results {
        requests += result.Requests
        success += result.Success
        networkFailed += result.NetworkFailed
        badFailed += result.BadFailed
    }

    elapsed := int64(time.Since(startTime).Seconds())

    if elapsed == 0 {
        elapsed = 1
    }

    fmt.Println()
    fmt.Printf("Requests:                       %10d hits\n", requests)
    fmt.Printf("Successful requests:            %10d hits\n", success)
    fmt.Printf("Network failed:                 %10d hits\n", networkFailed)
    fmt.Printf("Bad requests failed (!2xx):     %10d hits\n", badFailed)
    fmt.Printf("Successful requests rate:       %10d hits/sec\n", success/elapsed)
    fmt.Printf("Read throughput:                %10d bytes/sec\n", readThroughput/elapsed)
    fmt.Printf("Write throughput:               %10d bytes/sec\n", writeThroughput/elapsed)
    fmt.Printf("Test time:                      %10d sec\n", elapsed)
}

func readLines(path string) (lines []string, err error) {

    var file *os.File
    var part []byte
    var prefix bool

    if file, err = os.Open(path); err != nil {
        return
    }
    defer file.Close()

    reader := bufio.NewReader(file)
    buffer := bytes.NewBuffer(make([]byte, 0))
    for {
        if part, prefix, err = reader.ReadLine(); err != nil {
            break
        }
        buffer.Write(part)
        if !prefix {
            lines = append(lines, buffer.String())
            buffer.Reset()
        }
    }
    if err == io.EOF {
        err = nil
    }
    return
}

func NewConfiguration() *Configuration {

    if urlsFilePath == "" && url == "" {
        flag.Usage()
        os.Exit(1)
    }

    if requests == -1 && period == -1 {
        fmt.Println("Requests or period must be provided")
        flag.Usage()
        os.Exit(1)
    }

    if requests != -1 && period != -1 {
        fmt.Println("Only one should be provided: [requests|period]")
        flag.Usage()
        os.Exit(1)
    }

    configuration := &Configuration{
        urls:            make([]string, 0),
        method:          "GET",
        postData:        nil,
        keepAlive:       keepAlive,
        requests:        int64((1 << 63) - 1),
        authHeader:      authHeader,
        contentType:     contentType,
        apiUserName:     apiUserName,
        responseFileDir: responseFileDir}

    if period != -1 {
        configuration.period = period

        timeout := make(chan bool, 1)
        go func() {
            <-time.After(time.Duration(period) * time.Second)
            timeout <- true
        }()

        go func() {
            <-timeout
            if runtime.GOOS == "windows" {
                printResults(results, startTime)
                os.Exit(0)
            }
            pid := os.Getpid()
            proc, _ := os.FindProcess(pid)
            err := proc.Signal(os.Interrupt)
            if err != nil {
                log.Println(err)
                return
            }
        }()
    }

    if requests != -1 {
        configuration.requests = requests
    }

    if urlsFilePath != "" {
        fileLines, err := readLines(urlsFilePath)

        if err != nil {
            log.Fatalf("Error in ioutil.ReadFile for file: %s Error: ", urlsFilePath, err)
        }

        configuration.urls = fileLines
    }

    if url != "" {
        configuration.urls = append(configuration.urls, url)
    }

    if postDataFilePath != "" {
        configuration.method = "POST"

        data, err := ioutil.ReadFile(postDataFilePath)

        if err != nil {
            log.Fatalf("Error in ioutil.ReadFile for file path: %s Error: ", postDataFilePath, err)
        }

        configuration.postData = data
    }

    if configuration.responseFileDir != "" {
        responseFile, err := os.OpenFile(filepath.Join(configuration.responseFileDir, "responses.json"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
        if err != nil {
            log.Fatalf("Error opening response file: %v", err)
        }
        configuration.responseFile = responseFile
    }

    configuration.myClient.ReadTimeout = time.Duration(readTimeout) * time.Millisecond
    configuration.myClient.WriteTimeout = time.Duration(writeTimeout) * time.Millisecond
    configuration.myClient.MaxConnsPerHost = clients

    configuration.myClient.Dial = MyDialer()

    return configuration
}

func MyDialer() func(address string) (conn net.Conn, err error) {
    return func(address string) (net.Conn, error) {
        conn, err := net.Dial("tcp", address)
        if err != nil {
            return nil, err
        }

        myConn := &MyConn{Conn: conn}

        return myConn, nil
    }
}

func client(configuration *Configuration, result *Result, done *sync.WaitGroup) {
    for result.Requests < configuration.requests {
        for _, tmpUrl := range configuration.urls {

            data, err := ioutil.ReadFile("creditWallet_modified.json")
            if err != nil {
                log.Fatalf("Error in ioutil.ReadFile: %v", err)
            }
            randomTxnid := rand.Int31n(1<<31 - 1)
            updatedData := bytes.Replace(data, []byte("__TXNID__"), []byte(fmt.Sprintf("%d", randomTxnid)), 1)
            req := fasthttp.AcquireRequest()

            req.SetRequestURI(tmpUrl)
            req.Header.SetMethodBytes([]byte(configuration.method))

            if configuration.keepAlive == true {
                req.Header.Set("Connection", "keep-alive")
            } else {
                req.Header.Set("Connection", "close")
            }

            if len(configuration.authHeader) > 0 {
                req.Header.Set("Authorization", configuration.authHeader)
            }

            if len(configuration.contentType) > 0 {
                req.Header.Set("Content-Type", configuration.contentType)
            }
            if len(configuration.apiUserName) > 0 {
                req.Header.Set("apiUserName", configuration.apiUserName)
            }

            req.SetBody(updatedData)

            resp := fasthttp.AcquireResponse()
            err = configuration.myClient.Do(req, resp)
            statusCode := resp.StatusCode()
            result.Requests++

            if err != nil {
                result.NetworkFailed++
                if configuration.responseFile != nil {
                    responseData := ResponseData{
                        RequestNumber: result.Requests,
                        StatusCode:    statusCode,
                        TXN_ID:        randomTxnid,
                        ResponseData:  resp.Body(),
                    }
                    responseJSON, _ := json.Marshal(responseData)

                    // Append the response to the file
                    _, err := configuration.responseFile.WriteString(string(responseJSON) + "\n")
                    if err != nil {
                        fmt.Println(err)
                        continue
                    }
                }
                continue
            }

            if statusCode >= fasthttp.StatusOK && statusCode <= fasthttp.StatusIMUsed {
                result.Success++

            } else {
                result.BadFailed++
                if configuration.responseFile != nil {
                    responseData := ResponseData{
                        RequestNumber: result.Requests,
                        StatusCode:    statusCode,
                        TXN_ID:        randomTxnid,
                        ResponseData:  resp.Body(),
                    }
                    responseJSON, _ := json.Marshal(responseData)

                    // Append the response to the file
                    _, err := configuration.responseFile.WriteString(string(responseJSON) + "\n")
                    if err != nil {
                        fmt.Println(err)
                        continue
                    }
                }
            }

            fasthttp.ReleaseRequest(req)
            fasthttp.ReleaseResponse(resp)
        }
    }

    done.Done()
}

var results map[int]*Result = make(map[int]*Result)

var startTime time.Time

func main() {

    startTime = time.Now()
    var done sync.WaitGroup

    signalChannel := make(chan os.Signal, 2)
    signal.Notify(signalChannel, os.Interrupt)
    go func() {
        _ = <-signalChannel
        printResults(results, startTime)
        os.Exit(0)
    }()

    flag.Parse()

    configuration := NewConfiguration()

    goMaxProcs := os.Getenv("GOMAXPROCS")

    if goMaxProcs == "" {
        runtime.GOMAXPROCS(runtime.NumCPU())
    }

    fmt.Printf("Dispatching %d clients\n", clients)

    done.Add(clients)
    for i := 0; i < clients; i++ {
        result := &Result{}
        results[i] = result
        go client(configuration, result, &done)

    }
    fmt.Println("Waiting for results...")
    done.Wait()
    fmt.Println("wait is done")
    printResults(results, startTime)
}
