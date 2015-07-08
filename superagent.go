package main

import (
    "github.com/Jumpscale/jsagent/agent"
    "github.com/Jumpscale/jsagent/agent/lib/pm"
    "github.com/Jumpscale/jsagent/agent/lib/logger"
    "github.com/Jumpscale/jsagent/agent/lib/stats"
    "github.com/Jumpscale/jsagent/agent/lib/utils"
    _ "github.com/Jumpscale/jsagent/agent/lib/builtin"
    "github.com/shirou/gopsutil/process"
    "time"
    "encoding/json"
    "net/http"
    "log"
    "fmt"
    "strings"
    "bytes"
    "flag"
    "os"
    "io/ioutil"
)

func main() {
    settings := agent.Settings{}
    var cfg string
    var help bool

    flag.BoolVar(&help, "h", false, "Print this help screen")
    flag.StringVar(&cfg, "c", "", "Path to config file")
    flag.Parse()

    printHelp := func() {
        fmt.Println("agent [options]")
        flag.PrintDefaults()
    }

    if help {
        printHelp()
        return
    }

    if cfg == "" {
        fmt.Println("Missing required option -c")
        flag.PrintDefaults()
        os.Exit(1)
    }

    utils.LoadTomlFile(cfg, &settings)

    buildUrl := func (base string, endpoint string) string {
        base = strings.TrimRight(base, "/")
        return fmt.Sprintf("%s/%d/%d/%s", base,
            settings.Main.Gid,
            settings.Main.Nid,
            endpoint)
    }

    mgr := pm.NewPM(settings.Main.MessageIdFile)

    //apply logging handlers.
    for _, logcfg := range settings.Logging {
        switch strings.ToLower(logcfg.Type) {
            case "db":
                sqlFactory := logger.NewSqliteFactory(logcfg.LogDir)
                handler := logger.NewDBLogger(sqlFactory, logcfg.Levels)
                mgr.AddMessageHandler(handler.Log)
            case "ac":
                var endpoints []string

                if len(logcfg.AgentControllers) > 0 {
                    //specific ones.
                    endpoints = make([]string, 0, len(logcfg.AgentControllers))
                    for _, aci := range logcfg.AgentControllers {
                        endpoints = append(
                            endpoints,
                            buildUrl(settings.Main.AgentControllers[aci], "log"))
                    }
                } else {
                    //all ACs
                    endpoints = make([]string, 0, len(settings.Main.AgentControllers))
                    for _, ac := range settings.Main.AgentControllers {
                        endpoints = append(
                            endpoints,
                            buildUrl(ac, "log"))
                    }
                }

                batchsize := 1000 // default
                flushint := 120 // default (in seconds)
                if logcfg.BatchSize != 0 {
                    batchsize = logcfg.BatchSize
                }
                if logcfg.FlushInt != 0 {
                    flushint = logcfg.FlushInt
                }

                handler := logger.NewACLogger(
                    endpoints,
                    batchsize,
                    time.Duration(flushint) * time.Second,
                    logcfg.Levels)
                mgr.AddMessageHandler(handler.Log)
            case "console":
                handler := logger.NewConsoleLogger(logcfg.Levels)
                mgr.AddMessageHandler(handler.Log)
            default:
                panic(fmt.Sprintf("Unsupported logger type: %s", logcfg.Type))
        }
    }

    mgr.AddStatsdMeterHandler(func (statsd *stats.Statsd, cmd *pm.Cmd, ps *process.Process) {
        //for each long running external process this will be called every 2 sec
        //You can here collect all the data you want abou the process and feed
        //statsd.

        //TODO: Make sure this is the correct Base, key.
        base := fmt.Sprintf("%d.%d.%s.%s", cmd.Gid, cmd.Nid,
            cmd.Args.GetString("domain"), cmd.Args.GetString("name"))

        cpu, err := ps.CPUPercent(0)
        if err == nil {
            statsd.Avg(fmt.Sprintf("%s.cpu", base), cpu)
        }

        mem, err := ps.MemoryInfo()
        if err == nil {
            statsd.Avg(fmt.Sprintf("%s.rss", base), float64(mem.RSS))
            statsd.Avg(fmt.Sprintf("%s.vms", base), float64(mem.VMS))
            statsd.Avg(fmt.Sprintf("%s.swap", base), float64(mem.Swap))
        }
    })

    mgr.AddStatsFlushHandler(func (stats *stats.Stats) {
        //This will be called per process per stats_interval seconds. with
        //all the aggregated stats for that process.
        res, _ := json.Marshal(stats)
        log.Println(string(res))
        for _, base := range settings.Main.AgentControllers {
            url := buildUrl(base, "stats")

            reader := bytes.NewBuffer(res)
            resp, err := http.Post(url, "application/json", reader)
            if err != nil {
                log.Println("Failed to send stats result to AC", url, err)
                return
            }
            defer resp.Body.Close()
        }
    })

    //build list with ACs that we will poll from.
    var controllers []string
    if len(settings.Channel.Cmds) > 0 {
        controllers = make([]string, len(settings.Channel.Cmds))
        for i := 0; i < len(settings.Channel.Cmds); i++ {
            controllers[i] = settings.Main.AgentControllers[settings.Channel.Cmds[i]]
        }
    } else {
        controllers = settings.Main.AgentControllers
    }

    //start pollers goroutines
    for aci, ac := range controllers {
        go func() {
            lastfail := time.Now().Unix()
            for {
                response, err := http.Get(buildUrl(ac, "cmd"))
                if err != nil {
                    log.Println("Failed to retrieve new commands from", ac, err)
                    if time.Now().Unix() - lastfail < 4 {
                        time.Sleep(4 * time.Second)
                    }
                    lastfail = time.Now().Unix()

                    continue
                }

                defer response.Body.Close()
                body, err := ioutil.ReadAll(response.Body)
                if err != nil {
                    log.Println("Failed to load response content", err)
                    continue
                }

                cmd, err := pm.LoadCmd(body)
                if err != nil {
                    log.Println("Failed to load cmd", err)
                    continue
                }

                //set command defaults
                //1 - stats_interval
                meterInt := cmd.Args.GetInt("stats_interval")
                if meterInt == 0 {
                    cmd.Args.Set("stats_interval", settings.Stats.Interval)
                }

                //tag command for routing.
                cmd.Args.SetTag(aci)
                mgr.RunCmd(cmd)
            }
        } ()
    }

    //handle process results
    mgr.AddResultHandler(func (result *pm.JobResult) {
        //send result to AC.
        res, _ := json.Marshal(result)
        url := buildUrl(
            controllers[result.Args.GetTag()],
            "result")

        reader := bytes.NewBuffer(res)
        resp, err := http.Post(url, "application/json", reader)
        if err != nil {
            log.Println("Failed to send job result to AC", url, err)
            return
        }
        defer resp.Body.Close()
    })

    //register the execute commands
    for cmdKey, cmdCfg := range settings.Cmds {
        pm.RegisterCmd(cmdKey, cmdCfg.Binary, cmdCfg.Path, cmdCfg.Script)
    }

    //start process mgr.
    mgr.Run()

    //heart beat
    for {
        select {
        case <- time.After(10 * time.Second):
            log.Println("_/\\_ beep") // heart beat
        }
    }
}
