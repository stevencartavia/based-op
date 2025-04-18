import { customTestnetTokenToTokenItem } from '@/ui/utils/token';
import { findChain, isSameTesnetToken, updateChainStore } from '@/utils/chain';
import { CHAINS_ENUM } from '@debank/common';
import { GasLevel, Tx } from 'background/service/openapi';
import { createPersistStore, withTimeout } from 'background/utils';
import { BigNumber } from 'bignumber.js';
import { intToHex } from 'ethereumjs-util';
import { omitBy, sortBy } from 'lodash';
import { createClient, defineChain, erc20Abi, http, isAddress } from 'viem';
import {
  estimateGas,
  getBalance,
  getBlock,
  getGasPrice,
  getTransactionCount,
  getTransactionReceipt,
  readContract,
} from 'viem/actions';
import { http as axios } from '../utils/http';
import { matomoRequestEvent } from '@/utils/matomo-request';
import RPCService, { RPCServiceStore } from './rpc';
import { storage } from '../webapi';
import { ga4 } from '@/utils/ga4';

const MAX_READ_CONTRACT_TIME = 15_000;

export interface TestnetChainBase {
  id: number;
  name: string;
  nativeTokenSymbol: string;
  rpcUrl: string;
  scanLink?: string;
  hasPreconfs?: boolean;
}

export interface TestnetChain extends TestnetChainBase {
  nativeTokenAddress: string;
  hex: string;
  network: string;
  enum: CHAINS_ENUM;
  serverId: string;
  nativeTokenLogo: string;
  eip: Record<string, any>;
  nativeTokenDecimals: number;
  scanLink: string;
  isTestnet?: boolean;
  logo: string;
  whiteLogo?: string;
  needEstimateGas?: boolean;
  severity: number;
}

export interface RPCItem {
  url: string;
  enable: boolean;
}

export interface CustomTestnetTokenBase {
  id: string;
  chainId: number;
  symbol: string;
  decimals: number;
}

export interface CustomTestnetToken extends CustomTestnetTokenBase {
  amount: number;
  rawAmount: string;
  logo?: string;
}

export type CutsomTestnetServiceStore = {
  customTestnet: Record<string, TestnetChain>;
  customTokenList: CustomTestnetTokenBase[];
};

const MAX = 4_294_967_295;
let idCounter = Math.floor(Math.random() * MAX);

function getUniqueId(): number {
  idCounter = (idCounter + 1) % MAX;
  return idCounter;
}

class CustomTestnetService {
  store: CutsomTestnetServiceStore = {
    customTestnet: {},
    customTokenList: [],
  };

  chains: Record<string, ReturnType<typeof createClientByChain>> = {};

  logos: Record<
    string,
    {
      chain_logo_url: string;
      token_logo_url?: string;
    }
  > = {};

  init = async () => {
    const storageCache = await createPersistStore<CutsomTestnetServiceStore>({
      name: 'customTestnet',
      template: {
        customTestnet: {},
        customTokenList: [],
      },
    });
    this.store = storageCache || this.store;
    const coped = { ...this.store.customTestnet };
    Object.keys(coped).forEach((key) => {
      if (!/^\d+$/.test(key)) {
        delete coped[key];
      }
    });
    this.store.customTestnet = coped;
    const rpcStorage: RPCServiceStore = await storage.get('rpc');
    Object.values(this.store.customTestnet).forEach((chain) => {
      const config =
        rpcStorage.customRPC[chain.enum] &&
        rpcStorage.customRPC[chain.enum]?.enable
          ? { ...chain, rpcUrl: rpcStorage.customRPC[chain.enum].url }
          : chain;
      const client = createClientByChain(config);
      this.chains[chain.id] = client;
    });

    this.syncChainList();
    this.fetchLogos().then(() => {
      this.syncChainList();
    });
  };
  add = async (chain: TestnetChainBase) => {
    return this._update(chain, true);
  };

  update = async (chain: TestnetChainBase) => {
    return this._update(chain);
  };

  _update = async (chain: TestnetChainBase, isAdd?: boolean) => {
    chain.id = +chain.id;
    const local = findChain({
      id: +chain.id,
    });
    if (isAdd && local) {
      if (local.isTestnet) {
        return {
          error: {
            key: 'id',
            message: "You've already added this chain",
            status: 'alreadyAdded',
          },
        };
      } else {
        return {
          error: {
            key: 'id',
            message: 'Chain already integrated by Rabby Wallet',
            status: 'alreadySupported',
          },
        };
      }
    }
    try {
      const { data } = await axios.post(
        chain.rpcUrl,
        {
          jsonrpc: '2.0',
          id: getUniqueId(),
          method: 'eth_chainId',
        },
        {
          timeout: 6000,
        }
      );
      if (+data.result !== +chain.id) {
        return {
          error: {
            key: 'rpcUrl',
            message: 'RPC does not match the chainID',
          },
        };
      }
    } catch (error) {
      return {
        error: {
          key: 'rpcUrl',
          message: 'RPC invalid or currently unavailable',
        },
      };
    }
    const testnetChain = createTestnetChain(chain);
    this.store.customTestnet = {
      ...this.store.customTestnet,
      [chain.id]: testnetChain,
    };
    if (!RPCService.hasCustomRPC(testnetChain.enum)) {
      this.chains[chain.id] = createClientByChain(chain);
    }
    this.syncChainList();

    if (this.getList().length) {
      matomoRequestEvent({
        category: 'Custom Network',
        action: 'Custom Network Status',
        value: this.getList().length,
      });

      ga4.fireEvent('Has_CustomNetwork', {
        event_category: 'Custom Network',
      });
    }
    return this.store.customTestnet[chain.id];
  };

  remove = (chainId: number) => {
    this.store.customTestnet = omitBy(this.store.customTestnet, (item) => {
      return +chainId === +item.id;
    });
    this.store.customTokenList = this.store.customTokenList.filter((item) => {
      return +item.chainId !== +chainId;
    });
    delete this.chains[chainId];
    this.syncChainList();
    if (this.getList().length) {
      matomoRequestEvent({
        category: 'Custom Network',
        action: 'Custom Network Status',
        value: this.getList().length,
      });

      ga4.fireEvent('Has_CustomNetwork', {
        event_category: 'Custom Network',
      });
    }
  };

  getClient = (chainId: number) => {
    return this.chains[chainId];
  };

  getList = () => {
    const list = Object.values(this.store.customTestnet).map((item) => {
      const res = createTestnetChain(item);

      if (this.logos?.[res.id]) {
        res.logo = this.logos[res.id].chain_logo_url;
        res.nativeTokenLogo = this.logos[res.id].token_logo_url || '';
      }

      return res;
    });

    return list;
  };

  getTransactionReceipt = async ({
    chainId,
    hash,
  }: {
    chainId: number;
    hash: string;
  }) => {
    const client = this.getClient(+chainId);
    if (!client) {
      throw new Error(`Invalid chainId: ${chainId}`);
    }
    const res = await getTransactionReceipt(client, {
      hash: hash as any,
    });
    return {
      ...res,
      status: res.status === 'success' ? '0x1' : '0x0',
    };
  };

  getTx = ({ hash, chainId }: { chainId: number; hash: string }) => {
    const chain = findChain({ id: chainId });
    if (!chain) {
      throw new Error(`Invalid chainId: ${chainId}`);
    }
    return customTestnetService
      .getTransactionReceipt({
        chainId: chain!.id,
        hash: hash,
      })
      .then((res) => {
        return {
          ...res,
          hash: res.transactionHash,
          code: 0,
          status: 1,
          gas_used: Number(res.gasUsed),
          token: customTestnetTokenToTokenItem({
            amount: 0,
            symbol: chain.nativeTokenSymbol,
            decimals: chain.nativeTokenDecimals,
            id: chain.nativeTokenAddress,
            chainId: chain.id,
            rawAmount: '0',
            logo: this.logos?.[chain.id]?.token_logo_url,
          }),
        };
      })
      .catch((e) => {
        return {
          hash: hash,
          code: -1,
          status: 0,
          gas_used: 0,
          token: customTestnetTokenToTokenItem({
            amount: 0,
            symbol: chain.nativeTokenSymbol,
            decimals: chain.nativeTokenDecimals,
            id: chain.nativeTokenAddress,
            chainId: chain.id,
            rawAmount: '0',
            logo: this.logos?.[chain.id]?.token_logo_url,
          }),
        };
      });
  };

  getTransactionCount = ({
    address,
    blockTag,
    chainId,
  }: {
    address: string;
    blockTag: 'latest' | 'earliest' | 'pending' | 'safe' | 'finalized';
    chainId: number;
  }) => {
    const client = this.getClient(+chainId);
    if (!client) {
      throw new Error(`Invalid chainId: ${chainId}`);
    }
    return getTransactionCount(client, {
      address: address as any,
      blockTag,
    });
  };

  estimateGas = async ({
    address,
    tx,
    chainId,
  }: {
    address: string;
    tx: Tx;
    chainId: number;
  }) => {
    const client = this.getClient(+chainId);
    if (!client) {
      throw new Error(`Invalid chainId: ${chainId}`);
    }
    const res = await estimateGas(client, {
      account: address as any,
      ...tx,
    } as any);
    return res.toString();
  };

  getGasPrice = async (chainId: number) => {
    const client = this.getClient(+chainId);
    if (!client) {
      throw new Error(`Invalid chainId: ${chainId}`);
    }
    const res = await getGasPrice(client);
    return res.toString();
  };

  getBlockGasLimit = async (chainId: number) => {
    const client = this.getClient(+chainId);
    if (!client) {
      throw new Error(`Invalid chainId: ${chainId}`);
    }
    const res = await getBlock(client);
    return res.gasLimit.toString();
  };

  getGasMarket = async ({
    chainId,
    custom,
  }: {
    chainId: number;
    custom?: number;
  }) => {
    // const SETTINGS_BY_PRIORITY_LEVEL = {
    //   low: {
    //     percentile: 10 as Percentile,
    //     baseFeePercentageMultiplier: new BN(110),
    //     priorityFeePercentageMultiplier: new BN(94),
    //     minSuggestedMaxPriorityFeePerGas: new BN(1_000_000_000),
    //     estimatedWaitTimes: {
    //       minWaitTimeEstimate: 15_000,
    //       maxWaitTimeEstimate: 30_000,
    //     },
    //   },
    //   medium: {
    //     percentile: 20 as Percentile,
    //     baseFeePercentageMultiplier: new BN(120),
    //     priorityFeePercentageMultiplier: new BN(97),
    //     minSuggestedMaxPriorityFeePerGas: new BN(1_500_000_000),
    //     estimatedWaitTimes: {
    //       minWaitTimeEstimate: 15_000,
    //       maxWaitTimeEstimate: 45_000,
    //     },
    //   },
    //   high: {
    //     percentile: 30 as Percentile,
    //     baseFeePercentageMultiplier: new BN(125),
    //     priorityFeePercentageMultiplier: new BN(98),
    //     minSuggestedMaxPriorityFeePerGas: new BN(2_000_000_000),
    //     estimatedWaitTimes: {
    //       minWaitTimeEstimate: 15_000,
    //       maxWaitTimeEstimate: 60_000,
    //     },
    //   },
    // };

    const gasPrice = await this.getGasPrice(chainId);

    const levels = [
      {
        level: 'slow',
        baseFeePercentageMultiplier: 110,
        priorityFeePercentageMultiplier: 94,
      },
      {
        level: 'normal',
        baseFeePercentageMultiplier: 120,
        priorityFeePercentageMultiplier: 97,
      },
      {
        level: 'fast',
        baseFeePercentageMultiplier: 125,
        priorityFeePercentageMultiplier: 98,
      },
    ];

    return levels
      .map((item) => {
        return {
          level: item.level,
          price: new BigNumber(gasPrice)
            .multipliedBy(item.baseFeePercentageMultiplier)
            .div(100)
            .integerValue()
            .toNumber(),
          priority_price: Math.round(
            new BigNumber(gasPrice)
              .multipliedBy(item.priorityFeePercentageMultiplier)
              .div(100)
              .integerValue()
              .toNumber()
          ),
          front_tx_count: 0,
          estimated_seconds: 0,
        };
      })
      .concat([
        {
          level: 'custom',
          price: custom || 0,
          priority_price: custom || 0,
          front_tx_count: 0,
          estimated_seconds: 0,
        },
      ]) as GasLevel[];
  };

  addToken = (params: CustomTestnetTokenBase) => {
    if (this.hasToken(params)) {
      throw new Error('Token already added');
    }
    this.store.customTokenList = [...this.store.customTokenList, params];
  };

  removeToken = (params: CustomTestnetTokenBase) => {
    this.store.customTokenList = this.store.customTokenList.filter((item) => {
      return !isSameTesnetToken(item, params);
    });
  };

  hasToken = (params: Pick<CustomTestnetTokenBase, 'id' | 'chainId'>) => {
    return !!this.store.customTokenList.find((item) => {
      return isSameTesnetToken(params, item);
    });
  };

  getToken = async ({
    chainId,
    address,
    tokenId,
  }: {
    chainId: number;
    address: string;
    tokenId?: string | null;
  }): Promise<CustomTestnetToken> => {
    const [balance, tokenInfo] = await Promise.all([
      this.getBalance({
        chainId,
        address,
        tokenId,
      }),
      this.getTokenInfo({
        chainId,
        tokenId,
      }),
    ]);

    const { decimals } = tokenInfo;

    return {
      ...tokenInfo,
      amount: new BigNumber(balance.toString()).div(10 ** decimals).toNumber(),
      rawAmount: balance.toString(),
      logo:
        !tokenId || tokenId?.replace('custom_', '') === String(chainId)
          ? this.logos?.[chainId]?.token_logo_url
          : undefined,
    };
  };

  getBalance = async ({
    chainId,
    address,
    tokenId,
  }: {
    chainId: number;
    address: string;
    tokenId?: string | null;
  }) => {
    const client = this.getClient(+chainId);
    const chain = findChain({
      id: +chainId,
    });
    if (!client || !chain) {
      throw new Error(`Invalid chainId: ${chainId}`);
    }

    if (!tokenId || tokenId === chain.nativeTokenAddress) {
      const balance = await getBalance(client, {
        address: address as any,
      });
      return balance;
    }

    const balance = await readContract(client, {
      address: tokenId as any,
      abi: erc20Abi,
      functionName: 'balanceOf',
      args: [address as any],
    });

    return balance;
  };

  getTokenInfo = async ({
    chainId,
    tokenId,
  }: {
    chainId: number;
    tokenId?: string | null;
  }) => {
    const client = this.getClient(+chainId);
    const chain = findChain({
      id: +chainId,
    });
    if (!client || !chain) {
      throw new Error(`Invalid chainId: ${chainId}`);
    }

    if (!tokenId || tokenId === chain.nativeTokenAddress) {
      return {
        id: chain.nativeTokenAddress,
        symbol: chain.nativeTokenSymbol,
        chainId,
        decimals: chain.nativeTokenDecimals,
      };
    }

    const local = this.store.customTokenList?.find((item) => {
      return isSameTesnetToken(item, {
        id: tokenId,
        chainId,
      });
    });
    if (local) {
      return local;
    }

    // todo: multicall
    const [symbol, decimals] = await Promise.all([
      readContract(client, {
        address: tokenId as any,
        abi: erc20Abi,
        functionName: 'symbol',
      }),
      readContract(client, {
        address: tokenId as any,
        abi: erc20Abi,
        functionName: 'decimals',
      }),
    ]);

    return {
      id: tokenId,
      symbol: symbol,
      chainId,
      decimals,
    };
  };

  getTokenList = async ({
    address,
    chainId,
    q,
    isRemote,
  }: {
    address: string;
    chainId?: number;
    q?: string;
    isRemote?: boolean;
  }) => {
    const nativeTokenList = Object.values(this.store.customTestnet).map(
      (item) => {
        return {
          id: null,
          chainId: item.id,
          symbol: item.nativeTokenSymbol,
          logo: this.logos?.[item.id]?.token_logo_url,
        };
      }
    );
    const list = this.store.customTokenList || [];
    let tokenList = [...nativeTokenList, ...list];
    if (chainId) {
      tokenList = tokenList.filter((item) => {
        return item.chainId === chainId;
      });
    }

    if (q) {
      tokenList = tokenList.filter((item) => {
        return (
          item.id === q || item.symbol.toLowerCase().includes(q.toLowerCase())
        );
      });
    }
    let queryList = tokenList.map((item) => {
      return {
        tokenId: item.id,
        chainId: item.chainId,
        address,
      };
    });

    if (q && isAddress(q) && isRemote) {
      const chainList = chainId
        ? [chainId]
        : Object.values(this.store.customTestnet).map((item) => item.id);

      queryList = chainList.map((chainId) => {
        return {
          tokenId: q,
          chainId,
          address,
        };
      });
    }

    const res = await Promise.all(
      queryList.map((item) =>
        withTimeout(this.getToken(item), MAX_READ_CONTRACT_TIME).catch((e) => {
          console.error(e);
          return null;
        })
      )
    );
    return sortBy(
      res.filter((item): item is CustomTestnetToken => !!item),
      (item) => {
        return !item.id;
      },
      (item) => {
        return -item.amount;
      }
    );
  };

  // todo
  getTokenWithBalance = this.getTokenList;

  syncChainList = () => {
    const testnetList = this.getList();
    updateChainStore({
      testnetList: testnetList,
    });
  };

  fetchLogos = async () => {
    try {
      const { data } = await axios.get<typeof this.logos>(
        'https://static.debank.com/supported_testnet_chains.json'
      );
      this.logos = data;
      return data;
    } catch (e) {
      console.error(e);
      return {};
    }
  };

  setCustomRPC = ({ chainId, url }: { chainId: number; url: string }) => {
    const client = this.getClient(chainId);
    if (client) {
      this.chains[chainId] = createClientByChain({
        ...this.store.customTestnet[chainId],
        rpcUrl: url,
      });
    }
  };

  removeCustomRPC = (chainId: number) => {
    const client = this.getClient(chainId);
    if (client) {
      this.chains[chainId] = createClientByChain(
        this.store.customTestnet[chainId]
      );
    }
  };
}

export const customTestnetService = new CustomTestnetService();

const createClientByChain = (chain: TestnetChainBase) => {
  return createClient({
    chain: defineChain({
      id: chain.id,
      name: chain.name,
      nativeCurrency: {
        symbol: chain.nativeTokenSymbol,
        name: chain.nativeTokenSymbol,
        decimals: 18,
      },
      rpcUrls: {
        default: {
          http: [chain.rpcUrl],
        },
      },
    }),
    transport: http(chain.rpcUrl, { timeout: 30_000 }),
  });
};

export const createTestnetChain = (chain: TestnetChainBase): TestnetChain => {
  chain.id = +chain.id;
  return {
    ...chain,
    id: +chain.id,
    hex: intToHex(+chain.id),
    network: '' + chain.id,
    enum: `CUSTOM_${chain.id}` as CHAINS_ENUM,
    serverId: `custom_${chain.id}`,
    nativeTokenAddress: `custom_${chain.id}`,
    nativeTokenDecimals: 18,
    nativeTokenLogo: '',
    scanLink: chain.scanLink || '',
    logo: `data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 28 28'><circle cx='14' cy='14' r='14' fill='%236A7587'></circle><text x='14' y='15' dominant-baseline='middle' text-anchor='middle' fill='white' font-size='16' font-weight='500'>${encodeURIComponent(
      chain.name.trim().substring(0, 1).toUpperCase()
    )}</text></svg>`,
    eip: {
      1559: false,
    },
    isTestnet: true,
    severity: 0,
  };
};
