package main

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/filecoin-project/go-address"
	lapi "github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/types"
	lcli "github.com/filecoin-project/lotus/cli"
	actexported3 "github.com/filecoin-project/specs-actors/v3/actors/builtin/exported"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"github.com/urfave/cli/v2"
)

// change should wait for phase2.2 completed
func init() {
	address.CurrentNetwork = address.Mainnet
}

var log = logging.Logger("mpool-monitor")

func main() {
	logging.SetLogLevel("*", "ERROR")

	local := []*cli.Command{
		msgTypeCmd,
		mpoolLogCmd,
	}

	app := &cli.App{
		Name: "deal maker",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "repo",
				EnvVars: []string{"LOTUS_PATH"},
				Hidden:  true,
				Value:   "~/.lotus", // TODO: Consider XDG_DATA_HOME
			},
			&cli.StringFlag{
				Name:   "conf",
				Hidden: true,
				Value:  "./config.json", // TODO: Consider XDG_DATA_HOME
			},
		},
		Commands: local,
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println("Error: ", err)
		os.Exit(1)
	}
}

var mpoolLogCmd = &cli.Command{
	Name:  "mpool-log",
	Usage: "tell msg type",
	Flags: []cli.Flag{},
	Action: func(c *cli.Context) error {
		ctx := lcli.ReqContext(c)

		api, closer, err := lcli.GetFullNodeAPI(c)
		panicWhenError(err)
		defer closer()
		// poolMsgs, err := api.MpoolPending(ctx, types.EmptyTSK)
		// panicWhenError(err)
		// log.Infof("pool msgs count: %d", len(poolMsgs))
		methodNameList := make([]string, 0, 100)
		methodMsgMap := make(map[string]map[cid.Cid]*types.SignedMessage)
		lock := sync.Mutex{}

		populateMethodNames := func(name string) {
			for _, n := range methodNameList {
				if n == name {
					return
				}
			}
			methodNameList = append(methodNameList, name)
		}
		pendingMsgs, err := api.MpoolPending(ctx, types.EmptyTSK)
		panicWhenError(err)
		for _, msg := range pendingMsgs {
			mn, err := msgTypeName(ctx, api, &msg.Message)
			if err != nil {
				log.Warnf("unexpected: %v", err)
				continue
			}
			log.Infof("method: %s", mn)
			var msgsByCategory map[cid.Cid]*types.SignedMessage
			msgsByCategory, has := methodMsgMap[mn]
			if !has {
				msgsByCategory = make(map[cid.Cid]*types.SignedMessage)
				lock.Lock()
				methodMsgMap[mn] = msgsByCategory
				lock.Unlock()
			}
			populateMethodNames(mn)
			msgsByCategory[msg.Cid()] = msg
		}
		mpoolUpdate, err := api.MpoolSub(ctx)
		panicWhenError(err)
		go func(ctx context.Context) {
			for u := range mpoolUpdate {
				select {
				case <-ctx.Done():
					return
				default:
				}
				mn, err := msgTypeName(ctx, api, &u.Message.Message)
				if err != nil {
					log.Warnf("unexpected: %v", err)
					continue
				}
				log.Infof("method: %s", mn)
				var msgsByCategory map[cid.Cid]*types.SignedMessage
				msgsByCategory, has := methodMsgMap[mn]
				if !has {
					msgsByCategory = make(map[cid.Cid]*types.SignedMessage)
					lock.Lock()
					methodMsgMap[mn] = msgsByCategory
					lock.Unlock()
				}
				if u.Type == lapi.MpoolAdd {
					populateMethodNames(mn)
					msgsByCategory[u.Message.Cid()] = u.Message
				}
				if u.Type == lapi.MpoolRemove {
					lock.Lock()
					delete(msgsByCategory, u.Message.Cid())
					lock.Unlock()
				}
			}
		}(ctx)
		type logInfo struct {
			Name  string
			Count int
		}
		doLog := func() {

			infos := make([]logInfo, 0, 100)
			t := 0
			for name, mset := range methodMsgMap {
				st := len(mset)
				infos = append(infos, logInfo{name, st})
				t += st
				//fmt.Printf("%s count: %d\n", name, st)
			}
			sort.Slice(infos, func(i int, j int) bool {
				return infos[i].Name < infos[j].Name
			})
			//fmt.Printf("totoal msg: %d\n", t)
			fmt.Println("--------------------------------------")
			for _, item := range infos {
				fmt.Printf("%s count: %d\n", item.Name, item.Count)
			}
			fmt.Printf("totoal msg: %d\n", t)
			fmt.Println("======================================")
		}

		ticker := time.NewTicker(time.Second * 5)
		for {
			select {
			case <-ticker.C:
				doLog()
			case <-ctx.Done():
				return nil
			}
		}
		// mcid, err := cid.Decode(c.Args().First())
		// if err != nil {
		// 	return err
		// }
		// msg, err := api.ChainGetMessage(ctx, mcid)
		// if err != nil {
		// 	return err
		// }

	},
}

func msgTypeName(ctx context.Context, api lapi.FullNode, msg *types.Message) (string, error) {
	log.Infof("from: %v", msg.From)
	log.Infof("to: %v", msg.To)
	log.Infof("method: %v", msg.Method)
	if msg.Method == 0 {
		return "MethodSend", nil
	}
	actor, err := api.StateGetActor(ctx, msg.To, types.EmptyTSK)
	if err != nil {
		return "", err
	}

	for _, act := range actexported3.BuiltinActors() {
		if act.Code().Equals(actor.Code) {
			method := act.Exports()[msg.Method]
			av := reflect.ValueOf(method)
			if av.Kind() == reflect.Func {
				tstr := runtime.FuncForPC(av.Pointer()).Name()
				//fmt.Println(tstr)
				ei := strings.LastIndex(tstr, "-")
				si := strings.LastIndex(tstr, ".")
				return tstr[si+1 : ei], nil
			}
		}
	}
	return "unknown", nil
}
func panicWhenError(err error) {
	if err != nil {
		panic(err)
	}
}

var msgTypeCmd = &cli.Command{
	Name:  "msg-type",
	Usage: "tell msg type",
	Flags: []cli.Flag{},
	Action: func(c *cli.Context) error {
		ctx := lcli.ReqContext(c)

		api, closer, err := lcli.GetFullNodeAPI(c)
		if err != nil {
			return err
		}
		defer closer()
		mcid, err := cid.Decode(c.Args().First())
		if err != nil {
			return err
		}
		msg, err := api.ChainGetMessage(ctx, mcid)
		if err != nil {
			return err
		}
		log.Infof("from: %v", msg.From)
		log.Infof("to: %v", msg.To)
		log.Infof("method: %v", msg.Method)
		if msg.Method == 0 {
			fmt.Println("MethodSend")
			return nil
		}
		actor, err := api.StateGetActor(ctx, msg.To, types.EmptyTSK)
		if err != nil {
			return err
		}
		fmt.Println(actor)

		for _, act := range actexported3.BuiltinActors() {
			if act.Code().Equals(actor.Code) {
				method := act.Exports()[msg.Method]
				av := reflect.ValueOf(method)
				if av.Kind() == reflect.Func {
					tstr := runtime.FuncForPC(av.Pointer()).Name()

					fmt.Println(tstr)
				}
			}
		}
		return nil
	},
}
