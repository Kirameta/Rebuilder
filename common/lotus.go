package common

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Fogmeta/filecoin-ipfs-data-rebuilder/model"
	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/api/client"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type lotusClient struct {
	fullNodeApi string
	node        api.FullNode
	closer      jsonrpc.ClientCloser
}

func NewLotusClient(timeout ...int64) *lotusClient {
	var ctx context.Context
	if len(timeout) > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		defer cancel()
	} else {
		ctx = context.Background()
	}

	ainfo := ParseApiInfo(model.LotusSetting.FullNodeApi)
	addr, err := ainfo.DialArgs("v1")
	if err != nil {
		log.Errorf("init lotusClient could not get DialArgs: %w", err)
		return nil
	}
	//log.Infof("using raw API endpoint: %s", addr)
	fullNode, closer, err := client.NewFullNodeRPCV1(ctx, addr, ainfo.AuthHeader())
	if err != nil {
		if closer != nil {
			closer()
		}
		log.Errorf("init lotusClient could not get DialArgs: %v", err)
		return nil
	}
	return &lotusClient{
		node:   fullNode,
		closer: closer,
	}
}

func (lotus *lotusClient) GetMinerIdByPeerId(peerId string) (string, error) {
	//minerPeer, err := model.FindMinerPeer(peerId)
	//if err == nil && minerPeer != nil && minerPeer.PeerId != "" {
	//	return minerPeer.MinerId
	//} else {
	//	log.Warnf("not found minerId form database,peerId: %s, start update database from chain.", peerId)
	//	model.DeleteMinerPeer()
	//	updateMinerIAndPeerId()
	//	minerPeer, err = model.FindMinerPeer(peerId)
	//	if err != nil {
	//		log.Errorf("get minerpeer failed,error: %v", err)
	//		return ""
	//	}
	//	return minerPeer.MinerId
	//}

	minerPeer, err := model.FindMinerPeer(peerId)
	if err == nil && minerPeer != nil && minerPeer.PeerId != "" {
		return minerPeer.MinerId, nil
	} else {
		return "", err
	}
}

func SaveMinerIAndPeerId() {
	minerIdCh := make(chan string, 100)
	minerPeerCh := make(chan model.MinerPeer, 0)
	go func() {
		for minerId := range minerIdCh {
			peerId, err := NewLotusClient(10).GetMinerInfoByFId(minerId)
			if err != nil {
				log.Errorf("get minerInfo failed,error: %v", err)
				continue
			}
			minerPeerCh <- model.MinerPeer{
				MinerId: minerId,
				PeerId:  peerId,
			}
			time.Sleep(100 * time.Millisecond)
		}
		close(minerPeerCh)
	}()
	go func() {
		var count int8
		mp := make([]model.MinerPeer, 0)
		for minerPeer := range minerPeerCh {
			mp = append(mp, minerPeer)
			if count > 100 {
				model.InsertMinerPeers(mp)
				mp = make([]model.MinerPeer, 0)
			}
			count++
		}
		if len(mp) > 0 {
			model.InsertMinerPeers(mp)
		}
	}()

	miners := model.GetMiners()
	for _, miner := range miners {
		minerIdCh <- miner.MinerId
	}
	close(minerIdCh)
}

func SaveMinerByBrowser() {
	for page := 0; page < 500; page++ {
		log.Infof("page=%d", page)
		exitFlag, err := getMiner(page)
		time.Sleep(3 * time.Second)
		if err != nil {
			continue
		}
		if exitFlag {
			break
		}
	}
}

func getMiner(page int) (bool, error) {
	resp, err := http.Get(fmt.Sprintf("https://filfox.info/api/v1/miner/list/power?pageSize=50&page=%d", page))
	if err != nil {
		log.Errorf("get resp error: %v", err)
		return false, err
	}
	defer resp.Body.Close()
	if bytes, err := ioutil.ReadAll(resp.Body); err == nil {
		var minerData MinerData
		if err = json.Unmarshal(bytes, &minerData); err != nil {
			log.Errorf("resp to json error: %v", err)
			return false, err
		}

		if len(minerData.Miners) == 0 {
			return true, nil
		}

		mps := make([]model.Miner, 0)
		for _, addr := range minerData.Miners {
			mps = append(mps, model.Miner{
				MinerId: addr.Address,
			})
		}
		model.InsertMiners(mps)
	}
	return false, err
}

func (lotus *lotusClient) GetMinerInfoByFId(minerId string) (string, error) {
	defer lotus.closer()
	addr, _ := address.NewFromString(minerId)
	minerInfo, err := lotus.node.StateMinerInfo(context.TODO(), addr, types.EmptyTSK)
	if err != nil {
		log.Errorf("get minerInfo failed, minerId: %s,error: %v", addr.String(), err)
		return "", err
	}
	if minerInfo.PeerId == nil {
		return "", errors.New(fmt.Sprintf("minerId:[%s],peerId is nil", addr.String()))
	}
	return minerInfo.PeerId.String(), nil
}

func (lotus *lotusClient) ListMiners() ([]address.Address, error) {
	defer lotus.closer()
	return lotus.node.StateListMiners(context.TODO(), types.EmptyTSK)
}

func (lotus *lotusClient) getDealsCounts() (map[address.Address]int, error) {
	defer lotus.closer()
	allDeals, err := lotus.node.StateMarketDeals(context.TODO(), types.EmptyTSK)
	if err != nil {
		return nil, err
	}
	out := make(map[address.Address]int)
	for _, d := range allDeals {
		if d.State.SectorStartEpoch != -1 {
			out[d.Proposal.Provider]++
		}
	}
	return out, nil
}

func (lotus *lotusClient) RetrieveData(minerId, dataCid, savePath string) error {
	defer lotus.closer()
	log.Infof("start retrieve-data from minerId: %s,datacid: %s,savepath:%s", minerId, dataCid, savePath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	addr, err := address.NewFromString(minerId)
	if err != nil {
		log.Errorf("init address failed, minerId: %s,error: %v", minerId, err)
		return err
	}

	root, err := cid.Parse(dataCid)
	if err != nil {
		log.Errorf("parse cid failed , dataCid: %s,error: %v", dataCid, err)
		return err
	}
	offer, err := lotus.node.ClientMinerQueryOffer(context.TODO(), addr, root, nil)
	if err != nil {
		return err
	}

	var sel *api.Selector
	// wallet address
	pay, err := address.NewFromString(model.LotusSetting.Address)
	if err != nil {
		return err
	}
	o := offer.Order(pay)
	o.DataSelector = sel

	subscribeEvents, err := lotus.node.ClientGetRetrievalUpdates(context.TODO())
	if err != nil {
		return xerrors.Errorf("error setting up retrieval updates: %w", err)
	}

	retrievalRes, err := lotus.node.ClientRetrieve(context.TODO(), o)
	if err != nil {
		return xerrors.Errorf("error setting up retrieval: %w", err)
	}

	start := time.Now()
readEvents:
	for {
		var evt api.RetrievalInfo
		select {
		case <-ctx.Done():
			return xerrors.New("Retrieval Timed Out")
		case evt = <-subscribeEvents:
			if evt.ID != retrievalRes.DealID {
				continue
			}
		}

		event := "New"
		if evt.Event != nil {
			event = retrievalmarket.ClientEvents[*evt.Event]
		}

		log.Infof("Recv %s, Paid %s, %s (%s), %s\n",
			types.SizeStr(types.NewInt(evt.BytesReceived)),
			types.FIL(evt.TotalPaid),
			strings.TrimPrefix(event, "ClientEvent"),
			strings.TrimPrefix(retrievalmarket.DealStatuses[evt.Status], "DealStatus"),
			time.Now().Sub(start).Truncate(time.Millisecond),
		)

		switch evt.Status {
		case retrievalmarket.DealStatusCompleted:
			break readEvents
		case retrievalmarket.DealStatusRejected:
			return xerrors.Errorf("Retrieval Proposal Rejected: %s", evt.Message)
		case
			retrievalmarket.DealStatusDealNotFound,
			retrievalmarket.DealStatusErrored:
			return xerrors.Errorf("Retrieval Error: %s", evt.Message)
		}
	}

	err = lotus.node.ClientExport(ctx, api.ExportRef{
		Root:   root,
		DealID: retrievalRes.DealID,
	}, api.FileRef{
		Path:  savePath,
		IsCAR: false,
	})
	return err
}

func (lotus *lotusClient) GetCurrentHeight() (int64, error) {
	defer lotus.closer()
	tipSet, err := lotus.node.ChainHead(context.TODO())
	if err != nil {
		log.Errorf("get ChainHead failed,error: %v", err)
		return 0, err
	}
	return int64(tipSet.Height()), nil
}

func (lotus *lotusClient) Close() {
}

func ArchiveDir(src, out string) error {
	saveFile, err := os.Create(out)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(saveFile)

	filepath.Walk(src, func(file string, fi os.FileInfo, err error) error {
		header, err := tar.FileInfoHeader(fi, file)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(file)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !fi.IsDir() {
			data, err := os.Open(file)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, data); err != nil {
				return err
			}
		}
		return nil
	})
	if err := tw.Close(); err != nil {
		return err
	}
	return nil
}

type MinerData struct {
	Miners []struct {
		Address string `json:"address"`
	} `json:"miners"`
}
