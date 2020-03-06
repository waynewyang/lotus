package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"

	"github.com/filecoin-project/go-sectorbuilder"
	"github.com/filecoin-project/specs-actors/actors/builtin"
	"github.com/filecoin-project/specs-actors/actors/builtin/market"
	miner2 "github.com/filecoin-project/specs-actors/actors/builtin/miner"
	"github.com/filecoin-project/specs-actors/actors/builtin/power"
	crypto2 "github.com/filecoin-project/specs-actors/actors/crypto"
	"github.com/google/uuid"

	"github.com/filecoin-project/go-address"
	cborutil "github.com/filecoin-project/go-cbor-util"
	paramfetch "github.com/filecoin-project/go-paramfetch"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/ipfs/go-datastore"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/mitchellh/go-homedir"
	"golang.org/x/xerrors"
	"gopkg.in/urfave/cli.v2"

	lapi "github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/types"
	lcli "github.com/filecoin-project/lotus/cli"
	"github.com/filecoin-project/lotus/genesis"
	"github.com/filecoin-project/lotus/miner"
	"github.com/filecoin-project/lotus/node/config"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
	"github.com/filecoin-project/lotus/node/repo"
	"github.com/filecoin-project/lotus/storage"
	"github.com/filecoin-project/lotus/storage/sealing"
	"github.com/filecoin-project/lotus/storage/sealmgr/advmgr"
)

var initCmd = &cli.Command{
	Name:  "init",
	Usage: "Initialize a lotus storage miner repo",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "actor",
			Usage: "specify the address of an already created miner actor",
		},
		&cli.BoolFlag{
			Name:   "genesis-miner",
			Usage:  "enable genesis mining (DON'T USE ON BOOTSTRAPPED NETWORK)",
			Hidden: true,
		},
		&cli.BoolFlag{
			Name:  "create-worker-key",
			Usage: "create separate worker key",
		},
		&cli.StringFlag{
			Name:    "worker",
			Aliases: []string{"w"},
			Usage:   "worker key to use (overrides --create-worker-key)",
		},
		&cli.StringFlag{
			Name:    "owner",
			Aliases: []string{"o"},
			Usage:   "owner key to use",
		},
		&cli.Uint64Flag{
			Name:  "sector-size",
			Usage: "specify sector size to use",
			Value: uint64(build.SectorSizes[0]),
		},
		&cli.StringSliceFlag{
			Name:  "pre-sealed-sectors",
			Usage: "specify set of presealed sectors for starting as a genesis miner",
		},
		&cli.StringFlag{
			Name:  "pre-sealed-metadata",
			Usage: "specify the metadata file for the presealed sectors",
		},
		&cli.BoolFlag{
			Name:  "nosync",
			Usage: "don't check full-node sync status",
		},
		&cli.BoolFlag{
			Name:  "symlink-imported-sectors",
			Usage: "attempt to symlink to presealed sectors instead of copying them into place",
		},
		&cli.BoolFlag{
			Name:  "no-local-storage",
			Usage: "don't use storageminer repo for sector storage",
		},
	},
	Action: func(cctx *cli.Context) error {
		log.Info("Initializing lotus storage miner")

		ssize := abi.SectorSize(cctx.Uint64("sector-size"))

		symlink := cctx.Bool("symlink-imported-sectors")
		if symlink {
			log.Info("will attempt to symlink to imported sectors")
		}

		log.Info("Checking proof parameters")
		if err := paramfetch.GetParams(build.ParametersJson(), uint64(ssize)); err != nil {
			return xerrors.Errorf("fetching proof parameters: %w", err)
		}

		log.Info("Trying to connect to full node RPC")

		api, closer, err := lcli.GetFullNodeAPI(cctx) // TODO: consider storing full node address in config
		if err != nil {
			return err
		}
		defer closer()
		ctx := lcli.ReqContext(cctx)

		log.Info("Checking full node sync status")

		if !cctx.Bool("genesis-miner") && !cctx.Bool("nosync") {
			if err := lcli.SyncWait(ctx, api); err != nil {
				return xerrors.Errorf("sync wait: %w", err)
			}
		}

		log.Info("Checking if repo exists")

		repoPath := cctx.String(FlagStorageRepo)
		r, err := repo.NewFS(repoPath)
		if err != nil {
			return err
		}

		ok, err := r.Exists()
		if err != nil {
			return err
		}
		if ok {
			return xerrors.Errorf("repo at '%s' is already initialized", cctx.String(FlagStorageRepo))
		}

		log.Info("Checking full node version")

		v, err := api.Version(ctx)
		if err != nil {
			return err
		}

		if !v.APIVersion.EqMajorMinor(build.APIVersion) {
			return xerrors.Errorf("Remote API version didn't match (local %s, remote %s)", build.APIVersion, v.APIVersion)
		}

		log.Info("Initializing repo")

		if err := r.Init(repo.StorageMiner); err != nil {
			return err
		}

		{
			lr, err := r.Lock(repo.StorageMiner)
			if err != nil {
				return err
			}

			var sc config.StorageConfig

			if pssb := cctx.StringSlice("pre-sealed-sectors"); len(pssb) != 0 {
				log.Infof("Setting up storage config with presealed sector", pssb)

				for _, psp := range pssb {
					psp, err := homedir.Expand(psp)
					if err != nil {
						return err
					}
					sc.StoragePaths = append(sc.StoragePaths, config.LocalPath{
						Path: psp,
					})
				}
			}

			if !cctx.Bool("no-local-storage") {
				b, err := json.MarshalIndent(&config.StorageMeta{
					ID:       uuid.New().String(),
					Weight:   10,
					CanSeal:  true,
					CanStore: true,
				}, "", "  ")
				if err != nil {
					return xerrors.Errorf("marshaling storage config: %w", err)
				}

				if err := ioutil.WriteFile(filepath.Join(lr.Path(), "sectorstore.json"), b, 0644); err != nil {
					return xerrors.Errorf("persisting storage metadata (%s): %w", filepath.Join(lr.Path(), "storage.json"), err)
				}
			}

			sc.StoragePaths = append(sc.StoragePaths, config.LocalPath{
				Path: lr.Path(),
			})

			if err := lr.SetStorage(sc); err != nil {
				return xerrors.Errorf("set storage config: %w", err)
			}

			if err := lr.Close(); err != nil {
				return err
			}
		}

		if err := storageMinerInit(ctx, cctx, api, r, ssize); err != nil {
			log.Errorf("Failed to initialize lotus-storage-miner: %+v", err)
			path, err := homedir.Expand(repoPath)
			if err != nil {
				return err
			}
			log.Infof("Cleaning up %s after attempt...", path)
			if err := os.RemoveAll(path); err != nil {
				log.Errorf("Failed to clean up failed storage repo: %s", err)
			}
			return xerrors.Errorf("Storage-miner init failed")
		}

		// TODO: Point to setting storage price, maybe do it interactively or something
		log.Info("Storage miner successfully created, you can now start it with 'lotus-storage-miner run'")

		return nil
	},
}

func migratePreSealMeta(ctx context.Context, api lapi.FullNode, metadata string, maddr address.Address, mds dtypes.MetadataDS) error {
	metadata, err := homedir.Expand(metadata)
	if err != nil {
		return xerrors.Errorf("expanding preseal dir: %w", err)
	}

	b, err := ioutil.ReadFile(metadata)
	if err != nil {
		return xerrors.Errorf("reading preseal metadata: %w", err)
	}

	meta := genesis.Miner{}
	if err := json.Unmarshal(b, &meta); err != nil {
		return xerrors.Errorf("unmarshaling preseal metadata: %w", err)
	}

	maxSectorID := abi.SectorNumber(0)
	for _, sector := range meta.Sectors {
		sectorKey := datastore.NewKey(sealing.SectorStorePrefix).ChildString(fmt.Sprint(sector.SectorID))

		dealID, err := findMarketDealID(ctx, api, sector.Deal)
		if err != nil {
			return xerrors.Errorf("finding storage deal for pre-sealed sector %d: %w", sector.SectorID, err)
		}
		commD := sector.CommD
		commR := sector.CommR

		info := &sealing.SectorInfo{
			State:    lapi.Proving,
			SectorID: sector.SectorID,
			Pieces: []sealing.Piece{
				{
					DealID: &dealID,
					Size:   abi.PaddedPieceSize(meta.SectorSize).Unpadded(),
					CommP:  sector.CommD,
				},
			},
			CommD:            &commD,
			CommR:            &commR,
			Proof:            nil,
			Ticket:           lapi.SealTicket{},
			PreCommitMessage: nil,
			Seed:             lapi.SealSeed{},
			CommitMessage:    nil,
		}

		b, err := cborutil.Dump(info)
		if err != nil {
			return err
		}

		if err := mds.Put(sectorKey, b); err != nil {
			return err
		}

		if sector.SectorID > maxSectorID {
			maxSectorID = sector.SectorID
		}

		/* // TODO: Import deals into market
		pnd, err := cborutil.AsIpld(sector.Deal)
		if err != nil {
			return err
		}

		dealKey := datastore.NewKey(deals.ProviderDsPrefix).ChildString(pnd.Cid().String())

		deal := &deals.MinerDeal{
			MinerDeal: storagemarket.MinerDeal{
				ClientDealProposal: sector.Deal,
				ProposalCid: pnd.Cid(),
				State:       storagemarket.StorageDealActive,
				Ref:         &storagemarket.DataRef{Root: proposalCid}, // TODO: This is super wrong, but there
				// are no params for CommP CIDs, we can't recover unixfs cid easily,
				// and this isn't even used after the deal enters Complete state
				DealID: dealID,
			},
		}

		b, err = cborutil.Dump(deal)
		if err != nil {
			return err
		}

		if err := mds.Put(dealKey, b); err != nil {
			return err
		}*/
	}

	buf := make([]byte, binary.MaxVarintLen64)
	size := binary.PutUvarint(buf, uint64(maxSectorID+1))
	return mds.Put(datastore.NewKey("/storage/nextid"), buf[:size])
}

func findMarketDealID(ctx context.Context, api lapi.FullNode, deal market.DealProposal) (abi.DealID, error) {
	// TODO: find a better way
	//  (this is only used by genesis miners)

	deals, err := api.StateMarketDeals(ctx, types.EmptyTSK)
	if err != nil {
		return 0, xerrors.Errorf("getting market deals: %w", err)
	}

	for k, v := range deals {
		if v.Proposal.PieceCID.Equals(deal.PieceCID) {
			id, err := strconv.ParseUint(k, 10, 64)
			return abi.DealID(id), err
		}
	}

	return 0, xerrors.New("deal not found")
}

func storageMinerInit(ctx context.Context, cctx *cli.Context, api lapi.FullNode, r repo.Repo, ssize abi.SectorSize) error {
	lr, err := r.Lock(repo.StorageMiner)
	if err != nil {
		return err
	}
	defer lr.Close()

	log.Info("Initializing libp2p identity")

	p2pSk, err := makeHostKey(lr)
	if err != nil {
		return xerrors.Errorf("make host key: %w", err)
	}

	peerid, err := peer.IDFromPrivateKey(p2pSk)
	if err != nil {
		return xerrors.Errorf("peer ID from private key: %w", err)
	}

	mds, err := lr.Datastore("/metadata")
	if err != nil {
		return err
	}

	var addr address.Address
	if act := cctx.String("actor"); act != "" {
		a, err := address.NewFromString(act)
		if err != nil {
			return xerrors.Errorf("failed parsing actor flag value (%q): %w", act, err)
		}

		if cctx.Bool("genesis-miner") {
			if err := mds.Put(datastore.NewKey("miner-address"), a.Bytes()); err != nil {
				return err
			}

			ppt, spt, err := lapi.ProofTypeFromSectorSize(ssize)
			if err != nil {
				return err
			}

			smgr, err := advmgr.New(lr, &sectorbuilder.Config{
				SealProofType: spt,
				PoStProofType: ppt,
				Miner:         a,
			}, nil)
			if err != nil {
				return err
			}
			epp := storage.NewElectionPoStProver(smgr)

			m := miner.NewMiner(api, epp)
			{
				if err := m.Register(a); err != nil {
					return xerrors.Errorf("failed to start up genesis miner: %w", err)
				}

				defer func() {
					if err := m.Unregister(ctx, a); err != nil {
						log.Error("failed to shut down storage miner: ", err)
					}
				}()

				if err := configureStorageMiner(ctx, api, a, peerid); err != nil {
					return xerrors.Errorf("failed to configure storage miner: %w", err)
				}
			}

			return nil
		}

		if pssb := cctx.String("pre-sealed-metadata"); pssb != "" {
			pssb, err := homedir.Expand(pssb)
			if err != nil {
				return err
			}

			log.Infof("Importing pre-sealed sector metadata for %s", a)

			if err := migratePreSealMeta(ctx, api, pssb, a, mds); err != nil {
				return xerrors.Errorf("migrating presealed sector metadata: %w", err)
			}
		}

		if err := configureStorageMiner(ctx, api, a, peerid); err != nil {
			return xerrors.Errorf("failed to configure storage miner: %w", err)
		}

		addr = a
	} else {
		a, err := createStorageMiner(ctx, api, peerid, cctx)
		if err != nil {
			return xerrors.Errorf("creating miner failed: %w", err)
		}

		addr = a
	}

	log.Infof("Created new storage miner: %s", addr)
	if err := mds.Put(datastore.NewKey("miner-address"), addr.Bytes()); err != nil {
		return err
	}

	return nil
}

func makeHostKey(lr repo.LockedRepo) (crypto.PrivKey, error) {
	pk, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, err
	}

	ks, err := lr.KeyStore()
	if err != nil {
		return nil, err
	}

	kbytes, err := pk.Bytes()
	if err != nil {
		return nil, err
	}

	if err := ks.Put("libp2p-host", types.KeyInfo{
		Type:       "libp2p-host",
		PrivateKey: kbytes,
	}); err != nil {
		return nil, err
	}

	return pk, nil
}

func configureStorageMiner(ctx context.Context, api lapi.FullNode, addr address.Address, peerid peer.ID) error {
	waddr, err := api.StateMinerWorker(ctx, addr, types.EmptyTSK)
	if err != nil {
		return xerrors.Errorf("getWorkerAddr returned bad address: %w", err)
	}

	enc, err := actors.SerializeParams(&miner2.ChangePeerIDParams{NewID: peerid})
	if err != nil {
		return err
	}

	msg := &types.Message{
		To:       addr,
		From:     waddr,
		Method:   builtin.MethodsMiner.ChangePeerID,
		Params:   enc,
		Value:    types.NewInt(0),
		GasPrice: types.NewInt(0),
		GasLimit: types.NewInt(100000000),
	}

	smsg, err := api.MpoolPushMessage(ctx, msg)
	if err != nil {
		return err
	}

	log.Info("Waiting for message: ", smsg.Cid())
	ret, err := api.StateWaitMsg(ctx, smsg.Cid())
	if err != nil {
		return err
	}

	if ret.Receipt.ExitCode != 0 {
		return xerrors.Errorf("update peer id message failed with exit code %d", ret.Receipt.ExitCode)
	}

	return nil
}

func createStorageMiner(ctx context.Context, api lapi.FullNode, peerid peer.ID, cctx *cli.Context) (address.Address, error) {
	log.Info("Creating StorageMarket.CreateStorageMiner message")

	var err error
	var owner address.Address
	if cctx.String("owner") != "" {
		owner, err = address.NewFromString(cctx.String("owner"))
	} else {
		owner, err = api.WalletDefaultAddress(ctx)
	}
	if err != nil {
		return address.Undef, err
	}

	ssize := cctx.Uint64("sector-size")

	worker := owner
	if cctx.String("worker") != "" {
		worker, err = address.NewFromString(cctx.String("worker"))
	} else if cctx.Bool("create-worker-key") { // TODO: Do we need to force this if owner is Secpk?
		worker, err = api.WalletNew(ctx, crypto2.SigTypeBLS)
	}
	// TODO: Transfer some initial funds to worker
	if err != nil {
		return address.Undef, err
	}

	collateral, err := api.StatePledgeCollateral(ctx, types.EmptyTSK)
	if err != nil {
		return address.Undef, err
	}

	params, err := actors.SerializeParams(&power.CreateMinerParams{
		Owner:      owner,
		Worker:     worker,
		SectorSize: abi.SectorSize(ssize),
		Peer:       peerid,
	})
	if err != nil {
		return address.Undef, err
	}

	createStorageMinerMsg := &types.Message{
		To:    builtin.StoragePowerActorAddr,
		From:  owner,
		Value: types.BigAdd(collateral, types.BigDiv(collateral, types.NewInt(100))),

		Method: builtin.MethodsPower.CreateMiner,
		Params: params,

		GasLimit: types.NewInt(10000000),
		GasPrice: types.NewInt(0),
	}

	signed, err := api.MpoolPushMessage(ctx, createStorageMinerMsg)
	if err != nil {
		return address.Undef, err
	}

	log.Infof("Pushed StorageMarket.CreateStorageMiner, %s to Mpool", signed.Cid())
	log.Infof("Waiting for confirmation")

	mw, err := api.StateWaitMsg(ctx, signed.Cid())
	if err != nil {
		return address.Undef, err
	}

	if mw.Receipt.ExitCode != 0 {
		return address.Undef, xerrors.Errorf("create storage miner failed: exit code %d", mw.Receipt.ExitCode)
	}

	var retval power.CreateMinerReturn
	if err := retval.UnmarshalCBOR(bytes.NewReader(mw.Receipt.Return)); err != nil {
		return address.Undef, err
	}

	log.Infof("New storage miners address is: %s (%s)", retval.IDAddress, retval.RobustAddress)
	return retval.IDAddress, nil
}
