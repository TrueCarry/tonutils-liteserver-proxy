package server

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"github.com/kevinms/leakybucket-go"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"github.com/xssnick/tonutils-liteserver-proxy/config"
	"github.com/xssnick/tonutils-liteserver-proxy/internal/emulate"
	"github.com/xssnick/tonutils-liteserver-proxy/metrics"
	"net"
	"reflect"
	"time"
)

const HitTypeEmulated = "emulated"
const HitTypeBackend = "backend"
const HitTypeCache = "cache"
const HitTypeFailedValidate = "failed_validate"
const HitTypeFailedInternal = "failed_internal"

type Cache interface {
	GetTransaction(ctx context.Context, id *ton.BlockIDExt, account *ton.AccountID, lt int64) (*ton.TransactionInfo, bool, error)
	GetLibraries(ctx context.Context, hashes [][]byte) (*cell.Dictionary, bool, error)
	WaitMasterBlock(ctx context.Context, seqno uint32, timeout time.Duration) error
	GetZeroState() (*ton.ZeroStateIDExt, error)
	GetMasterBlock(ctx context.Context, id *ton.BlockIDExt) (*MasterBlock, bool, error)
	GetLastMasterBlock(ctx context.Context) (*MasterBlock, bool, error)
	GetBlock(ctx context.Context, id *ton.BlockIDExt) (*ton.BlockData, bool, error)
	GetAccountState(ctx context.Context, block *MasterBlock, addr *address.Address) (*ton.AccountState, bool, error)
}

type Client struct {
	processor chan *liteclient.LiteServerQuery
}

type ProxyBalancer struct {
	srv             *liteclient.Server
	backendBalancer *BackendBalancer

	cache     Cache
	configs   map[string]*KeyConfig
	onlyProxy bool
}

type KeyConfig struct {
	name          string
	limiterPerIP  *leakybucket.Collector
	limiterPerKey *leakybucket.LeakyBucket
}

func NewProxyBalancer(configs []config.ClientConfig, backendBalancer *BackendBalancer, cache Cache, onlyProxy bool) *ProxyBalancer {
	s := &ProxyBalancer{
		backendBalancer: backendBalancer,
		configs:         map[string]*KeyConfig{},
		cache:           cache,
		onlyProxy:       onlyProxy,
	}

	var keys []ed25519.PrivateKey

	for _, cfg := range configs {
		key := ed25519.NewKeyFromSeed(cfg.PrivateKey)
		keys = append(keys, key)

		var keyCfg KeyConfig
		keyCfg.name = cfg.Name
		if cfg.CapacityPerKey > 0 {
			keyCfg.limiterPerKey = leakybucket.NewLeakyBucket(cfg.CoolingPerSec, cfg.CapacityPerKey)
		}
		if cfg.CapacityPerIP > 0 {
			keyCfg.limiterPerIP = leakybucket.NewCollector(cfg.CoolingPerSec, cfg.CapacityPerIP, true)
		}

		s.configs[string(key.Public().(ed25519.PublicKey))] = &keyCfg
	}
	s.srv = liteclient.NewServer(keys)

	s.srv.SetMessageHandler(s.handleRequest)
	s.srv.SetConnectionHook(func(conn net.Conn) error {
		// TODO: ip filtering?
		log.Debug().Str("addr", conn.RemoteAddr().String()).Msg("new client connected")
		metrics.Global.ActiveADNLConnections.Add(1)
		return nil
	})
	s.srv.SetDisconnectHook(func(ctx context.Context, client *liteclient.ServerClient) {
		log.Debug().Str("addr", client.IP()).Msg("client disconnected")
		metrics.Global.ActiveADNLConnections.Sub(1)
	})
	return s
}

func (s *ProxyBalancer) Listen(addr string) error {
	return s.srv.Listen(addr)
}

func (s *ProxyBalancer) handleRequest(ctx context.Context, sc *liteclient.ServerClient, msg tl.Serializable) error {
	lim := s.configs[string(sc.ServerKey())]
	keyName := "unknown"
	if lim != nil {
		keyName = lim.name
	}

	defer func() {
		metrics.Global.Requests.WithLabelValues(keyName, reflect.TypeOf(msg).String()).Add(1)
	}()

	switch m := msg.(type) {
	case adnl.MessageQuery:
		switch q := m.Data.(type) {
		case liteclient.LiteServerQuery:
			if lim == nil {
				return sc.Send(adnl.MessageAnswer{ID: m.ID, Data: ton.LSError{
					Code: 401,
					Text: "unexpected server key",
				}})
			}

			cost := int64(1) // TODO: dynamic cost (depending on query)

			if lim.limiterPerIP != nil && lim.limiterPerIP.Add(sc.IP(), cost) != cost {
				return sc.Send(adnl.MessageAnswer{ID: m.ID, Data: ton.LSError{
					Code: 429,
					Text: "too many requests",
				}})
			}
			if lim.limiterPerKey != nil && lim.limiterPerKey.Add(cost) != cost {
				return sc.Send(adnl.MessageAnswer{ID: m.ID, Data: ton.LSError{
					Code: 429,
					Text: "too many requests",
				}})
			}

			go func() {
				var resp tl.Serializable

				if !s.onlyProxy {
					switch v := q.Data.(type) {
					case []tl.Serializable: // wait master probably
						if len(v) != 2 {
							_ = sc.Send(adnl.MessageAnswer{ID: m.ID, Data: ton.LSError{
								Code: 400,
								Text: "unexpected len of queries",
							}})
							return
						}

						wt, ok := v[0].(ton.WaitMasterchainSeqno)
						if !ok {
							_ = sc.Send(adnl.MessageAnswer{ID: m.ID, Data: ton.LSError{
								Code: 400,
								Text: "unexpected first query type",
							}})
							return
						}

						tm := time.Now()
						if err := s.cache.WaitMasterBlock(ctx, uint32(wt.Seqno), time.Duration(wt.Timeout)*time.Second); err != nil {
							if ls, ok := err.(ton.LSError); ok {
								_ = sc.Send(adnl.MessageAnswer{ID: m.ID, Data: ls})
								return
							}
							return
						}
						log.Debug().Dur("took", time.Since(tm)).Msg("master block wait finished")
						q.Data = v[1]
					}

					hitType := HitTypeBackend
					tm := time.Now()
					defer func() {
						if ls, ok := resp.(ton.LSError); ok {
							metrics.Global.LSErrors.WithLabelValues(keyName, reflect.TypeOf(q.Data).String(), fmt.Sprint(ls.Code)).Add(1)
						}

						snc := time.Since(tm)
						metrics.Global.Queries.WithLabelValues(keyName, reflect.TypeOf(q.Data).String(), hitType).Observe(snc.Seconds())
						log.Debug().Type("request", q.Data).Dur("took", snc).Msg("query finished")
					}()

					switch v := q.Data.(type) {
					case ton.GetVersion:
						hitType = HitTypeEmulated
						resp = ton.Version{
							Mode:         0,
							Version:      0x101,
							Capabilities: 7,
							Now:          uint32(time.Now().Unix()),
						}
					case ton.GetTime:
						hitType = HitTypeEmulated
						resp = ton.CurrentTime{
							Now: uint32(time.Now().Unix()),
						}
					case ton.GetMasterchainInfoExt:
						resp, hitType = s.handleGetMasterchainInfoExt(ctx, &v)
					case ton.GetMasterchainInf:
						resp, hitType = s.handleGetMasterchainInfo(ctx)
					case ton.GetLibraries:
						resp, hitType = s.handleGetLibraries(ctx, &v)
					case ton.GetOneTransaction:
						resp, hitType = s.handleGetTransaction(ctx, &v)
					case ton.GetBlockData:
						resp, hitType = s.handleGetBlock(ctx, &v)
					case ton.GetAccountState:
						resp, hitType = s.handleGetAccount(ctx, &v)
					case ton.RunSmcMethod:
						resp, hitType = s.handleRunSmcMethod(ctx, &v)
					case ton.GetConfigAll:
					case ton.GetBlockProof:
					case ton.GetConfigParams:
					case ton.GetBlockHeader:
					case ton.LookupBlock:
					case ton.GetAllShardsInfo:
					case ton.ListBlockTransactions:
					case ton.ListBlockTransactionsExt:
						// TODO: cache all of this
					}
				}

				if resp == nil {
					log.Debug().Type("request", q.Data).Msg("direct proxy")
					// we expect to have only fast nodes, so timeout is short
					ctx, cancel := context.WithTimeout(ctx, 7*time.Second)

					tm := time.Now()
					err := s.backendBalancer.GetClient().QueryLiteserver(ctx, q.Data, &resp)
					cancel()
					if err != nil {
						if ls, ok := err.(ton.LSError); ok {
							resp = ls
						} else {
							log.Warn().Err(err).Type("request", q.Data).Dur("took", time.Since(tm)).Msg("query failed")

							resp = ton.LSError{
								Code: 502,
								Text: "backend node timeout",
							}
						}
					}
				}

				_ = sc.Send(adnl.MessageAnswer{ID: m.ID, Data: resp})
			}()

			return nil
		}
	case liteclient.TCPPing:
		return sc.Send(liteclient.TCPPong{RandomID: m.RandomID})
	}

	return fmt.Errorf("something unknown: %s", reflect.TypeOf(msg).String())
}

func (s *ProxyBalancer) handleRunSmcMethod(ctx context.Context, v *ton.RunSmcMethod) (tl.Serializable, string) {
	block, cachedMaster, err := s.cache.GetMasterBlock(ctx, v.ID)
	if err != nil {
		if ls, ok := err.(ton.LSError); ok {
			return ls, HitTypeFailedValidate
		}

		log.Warn().Err(err).Type("request", v).Msg("failed to get master block")
		return ton.LSError{
			Code: 500,
			Text: "failed to resolve master block",
		}, HitTypeFailedInternal
	}

	addr := address.NewAddress(0, byte(v.Account.Workchain), v.Account.ID)
	state, cachedState, err := s.cache.GetAccountState(ctx, block, addr)
	if err != nil {
		if ls, ok := err.(ton.LSError); ok {
			return ls, HitTypeFailedValidate
		}

		log.Warn().Err(err).Type("request", v).Msg("failed to get account")

		return ton.LSError{
			Code: 500,
			Text: "failed to get account state",
		}, HitTypeFailedInternal
	}

	if state.State == nil {
		return ton.LSError{
			Code: ton.ErrCodeContractNotInitialized,
			Text: "contract is not initialized",
		}, HitTypeFailedValidate
	}

	var st tlb.AccountState
	if err = st.LoadFromCell(state.State.BeginParse()); err != nil {
		log.Warn().Err(err).Type("request", v).Msg("failed to parse account")
		return ton.LSError{
			Code: 500,
			Text: "failed to parse account state: " + err.Error(),
		}, HitTypeFailedInternal
	}

	libsCodes, cachedLibs, err := s.cache.GetLibraries(ctx, findLibs(st.StateInit.Code))
	if err != nil {
		if ls, ok := err.(ton.LSError); ok {
			return ls, HitTypeFailedValidate
		}

		return ton.LSError{
			Code: 500,
			Text: "failed resolve libraries: " + err.Error(),
		}, HitTypeFailedInternal
	}

	// TODO: precompiled contracts in go

	etm := time.Now()
	res, err := emulate.RunGetMethod(int32(v.MethodID), emulate.RunMethodParams{
		Code:    st.StateInit.Code,
		Data:    st.StateInit.Data,
		Address: addr,
		Stack:   v.Params,
		Balance: st.Balance.Nano(),
		Libs:    libsCodes,
		Config:  block.Config,
		Time:    time.Now(),
	}, false, 1_000_000)
	if err != nil {
		log.Warn().Err(err).Type("request", v).Msg("failed to emulate get method")

		return ton.LSError{
			Code: 500,
			Text: "failed to emulate run method: " + err.Error(),
		}, HitTypeFailedInternal
	}
	log.Debug().Dur("took", time.Since(etm)).Msg("get method emulation finished")

	var stateProof, c7 *cell.Cell
	if v.Mode&8 != 0 {
		//TODO: support c7 return
		return ton.LSError{
			Code: 403,
			Text: "c7 return is currently not supported",
		}, HitTypeFailedValidate + "_want_c7"
	}

	if v.Mode&2 != 0 {
		stateProof, err = state.State.CreateProof(cell.CreateProofSkeleton())
		if err != nil {
			log.Warn().Err(err).Type("request", v).Msg("failed to prepare state proof args")

			return ton.LSError{
				Code: 500,
				Text: "failed to prepare state proof args: " + err.Error(),
			}, HitTypeFailedInternal
		}
	}

	hit := HitTypeBackend
	if cachedMaster && cachedLibs {
		hit = HitTypeEmulated
		if cachedState {
			hit = HitTypeCache
		}
	}

	return ton.RunMethodResult{
		Mode:       v.Mode,
		ID:         v.ID,
		ShardBlock: state.Shard,
		ShardProof: state.ShardProof,
		Proof:      state.Proof,
		StateProof: stateProof,
		InitC7:     c7,
		LibExtras:  nil,
		ExitCode:   res.ExitCode,
		Result:     res.Stack,
	}, hit
}

func (s *ProxyBalancer) handleGetMasterchainInfoExt(ctx context.Context, v *ton.GetMasterchainInfoExt) (tl.Serializable, string) {
	if v.Mode != 0 {
		return ton.LSError{
			Code: 400,
			Text: "non zero mode is not supported",
		}, HitTypeFailedValidate
	}

	block, cached, err := s.cache.GetLastMasterBlock(ctx)
	if err != nil {
		if ls, ok := err.(ton.LSError); ok {
			return ls, HitTypeFailedInternal
		}

		log.Warn().Err(err).Type("request", v).Msg("failed to get last master")
		return ton.LSError{
			Code: 500,
			Text: "failed to resolve master block",
		}, HitTypeFailedInternal
	}

	zero, err := s.cache.GetZeroState()
	if err != nil {
		log.Warn().Err(err).Type("request", v).Msg("failed to get zero state")

		return ton.LSError{
			Code: 500,
			Text: "failed to resolve zero state",
		}, HitTypeFailedInternal
	}

	hit := HitTypeBackend
	if cached {
		hit = HitTypeCache
	}

	return ton.MasterchainInfoExt{
		Mode:          v.Mode,
		Version:       0x101,
		Capabilities:  7,
		Last:          block.Block.ID,
		LastUTime:     block.GenTime,
		Now:           uint32(time.Now().Unix()),
		StateRootHash: block.StateHash,
		Init:          zero,
	}, hit
}

func (s *ProxyBalancer) handleGetMasterchainInfo(ctx context.Context) (tl.Serializable, string) {
	block, cached, err := s.cache.GetLastMasterBlock(ctx)
	if err != nil {
		if ls, ok := err.(ton.LSError); ok {
			return ls, HitTypeFailedInternal
		}

		log.Warn().Err(err).Type("request", ton.GetMasterchainInf{}).Msg("failed to get last master")
		return ton.LSError{
			Code: 500,
			Text: "failed to resolve master block",
		}, HitTypeFailedInternal
	}

	zero, err := s.cache.GetZeroState()
	if err != nil {
		log.Warn().Err(err).Type("request", ton.GetMasterchainInf{}).Msg("failed to get zero state")

		return ton.LSError{
			Code: 500,
			Text: "failed to resolve zero state",
		}, HitTypeFailedInternal
	}

	hit := HitTypeBackend
	if cached {
		hit = HitTypeCache
	}
	return ton.MasterchainInfo{
		Last:          block.Block.ID,
		StateRootHash: block.StateHash,
		Init:          zero,
	}, hit
}

func (s *ProxyBalancer) handleGetLibraries(ctx context.Context, v *ton.GetLibraries) (tl.Serializable, string) {
	libs, cached, err := s.cache.GetLibraries(ctx, v.LibraryList)
	if err != nil {
		if ls, ok := err.(ton.LSError); ok {
			return ls, HitTypeFailedValidate
		}

		log.Warn().Err(err).Type("request", v).Msg("failed to get libraries")
		return ton.LSError{
			Code: 500,
			Text: "failed to get libraries",
		}, HitTypeFailedInternal
	}

	all, err := libs.LoadAll()
	if err != nil {
		log.Warn().Err(err).Type("request", v).Msg("failed to load libraries")
		return ton.LSError{
			Code: 500,
			Text: "failed to load libraries",
		}, HitTypeFailedInternal
	}

	var libsRes []*ton.LibraryEntry
	for _, kv := range all {
		libsRes = append(libsRes, &ton.LibraryEntry{
			Hash: kv.Key.MustLoadSlice(256),
			Data: kv.Value.MustToCell(),
		})
	}

	hit := HitTypeBackend
	if cached {
		hit = HitTypeCache
	}

	return ton.LibraryResult{
		Result: libsRes,
	}, hit
}

func (s *ProxyBalancer) handleGetBlock(ctx context.Context, v *ton.GetBlockData) (tl.Serializable, string) {
	data, cached, err := s.cache.GetBlock(ctx, v.ID)
	if err != nil {
		if ls, ok := err.(ton.LSError); ok {
			return ls, HitTypeFailedValidate
		}

		log.Warn().Err(err).Type("request", v).Msg("failed to get block")
		return ton.LSError{
			Code: 500,
			Text: "failed to get block",
		}, HitTypeFailedInternal
	}

	if cached {
		return data, HitTypeCache
	}
	return data, HitTypeBackend
}

func (s *ProxyBalancer) handleGetTransaction(ctx context.Context, v *ton.GetOneTransaction) (tl.Serializable, string) {
	data, cached, err := s.cache.GetTransaction(ctx, v.ID, v.AccID, v.LT)
	if err != nil {
		if ls, ok := err.(ton.LSError); ok {
			return ls, HitTypeFailedValidate
		}

		log.Warn().Err(err).Type("request", v).Msg("failed to get transaction")
		return ton.LSError{
			Code: 500,
			Text: "failed to get transaction",
		}, HitTypeFailedInternal
	}

	if cached {
		return data, HitTypeEmulated
	}
	return data, HitTypeBackend
}

func (s *ProxyBalancer) handleGetAccount(ctx context.Context, v *ton.GetAccountState) (tl.Serializable, string) {
	block, cachedBlock, err := s.cache.GetMasterBlock(ctx, v.ID)
	if err != nil {
		if ls, ok := err.(ton.LSError); ok {
			return ls, HitTypeFailedValidate
		}

		log.Warn().Err(err).Type("request", v).Msg("failed to get master block")
		return ton.LSError{
			Code: 500,
			Text: "failed to resolve master block",
		}, HitTypeFailedInternal
	}

	state, cachedState, err := s.cache.GetAccountState(ctx, block, address.NewAddress(0, byte(v.Account.Workchain), v.Account.ID))
	if err != nil {
		if ls, ok := err.(ton.LSError); ok {
			return ls, HitTypeFailedValidate
		}

		log.Warn().Err(err).Type("request", v).Msg("failed to get account state")
		return ton.LSError{
			Code: 500,
			Text: "failed to get account state",
		}, HitTypeFailedInternal
	}

	if cachedState && cachedBlock {
		return state, HitTypeCache
	}
	return state, HitTypeBackend
}

func findLibs(code *cell.Cell) (res [][]byte) {
	if code.RefsNum() == 0 && code.GetType() == cell.LibraryCellType {
		slc := code.BeginParse()
		slc.MustLoadSlice(8)
		return [][]byte{slc.MustLoadSlice(256)}
	}

	for i := 0; i < int(code.RefsNum()); i++ {
		res = append(res, findLibs(code.MustPeekRef(i))...)
	}
	return res
}