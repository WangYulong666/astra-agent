// 链路判定的断言：直连 / 经中继 / 未配置 / 隧道断开。
//
// 这一组错了的后果不是「界面难看」，而是**排障指错方向**：
// 把「relay 没连上」显示成「远程桌面未启用」，用户会去翻服务日志；
// 把「经中继」显示成「直连」，用户会去查家里的防火墙。所以逐条钉住。

import { describe, expect, it } from 'vitest'
import {
  NO_RELAY_HINT,
  describeLink,
  detectLinkKind,
  isLanHost,
  normalizeTunnel,
  relayAccessUrl,
  relayDeviceFromPath,
  transportLabel,
} from './remoteLink'
import type { LinkLocation, TunnelState } from './remoteLink'

const direct: LinkLocation = {
  protocol: 'http:',
  host: '192.168.1.20:7788',
  pathname: '/',
}

const viaRelay: LinkLocation = {
  protocol: 'https:',
  host: 'relay.example.com',
  pathname: '/t/mac-mini-a1b2c3/',
}

function tunnel(patch: Partial<TunnelState> = {}): TunnelState {
  return {
    configured: true,
    connected: true,
    url: 'https://relay.example.com',
    deviceId: 'mac-mini-a1b2c3',
    streams: 0,
    reconnects: 0,
    lastError: '',
    ...patch,
  }
}

describe('detectLinkKind', () => {
  it('局域网根路径 = 直连', () => {
    expect(detectLinkKind(direct, '')).toBe('direct')
    expect(detectLinkKind({ ...direct, host: '127.0.0.1:5173' }, '')).toBe('direct')
  })

  it('路径带 /t/<device>/ = 经中继', () => {
    expect(detectLinkKind(viaRelay)).toBe('relay')
    // relay 会把前缀剥掉，但页面自己仍在这个前缀下
    expect(detectLinkKind({ ...viaRelay, pathname: '/t/dev_7c1f/admin/remote' })).toBe('relay')
    // 没有结尾斜杠也算（relay 原样转发，路径可能不带尾斜杠）
    expect(detectLinkKind({ ...viaRelay, pathname: '/t/dev_7c1f' })).toBe('relay')
  })

  it('host 就是 relay 的公网地址 = 经中继（自定义前缀/反代场景）', () => {
    expect(
      detectLinkKind({ protocol: 'https:', host: 'relay.example.com', pathname: '/' }, 'https://relay.example.com'),
    ).toBe('relay')
  })

  it('relay 地址写法不同（尾斜杠/端口）也能对上 host', () => {
    expect(
      detectLinkKind({ protocol: 'https:', host: 'relay.example.com:9443', pathname: '/' }, 'wss://relay.example.com:9443'),
    ).toBe('relay')
  })

  it('/tool 这种前缀不会被误判成设备（前缀必须完整匹配 /t/）', () => {
    expect(detectLinkKind({ ...direct, pathname: '/tool/x' })).toBe('direct')
    expect(relayDeviceFromPath('/tool/x')).toBe('')
  })

  it('设备 id 会 decode（relay 允许 . _ - 但也可能被编码）', () => {
    expect(relayDeviceFromPath('/t/dev_7c1f%2Fa/')).toBe('dev_7c1f/a')
  })
})

describe('isLanHost', () => {
  it('私有网段与回环都算局域网', () => {
    for (const h of ['localhost:7788', '127.0.0.1', '[::1]:5173', '192.168.0.9', '10.1.2.3', '172.20.1.1', 'mac-mini.local:7788']) {
      expect(isLanHost(h)).toBe(true)
    }
  })

  it('公网域名不算', () => {
    for (const h of ['relay.example.com', '172.32.0.1', '8.8.8.8']) {
      expect(isLanHost(h)).toBe(false)
    }
  })
})

describe('normalizeTunnel', () => {
  it('未配置时后端只回 {configured:false}，其余字段补默认值', () => {
    expect(normalizeTunnel({ configured: false })).toEqual({
      configured: false,
      connected: false,
      url: '',
      deviceId: '',
      streams: 0,
      reconnects: 0,
      lastError: '',
    })
  })

  it('垃圾输入不会把界面带崩', () => {
    expect(normalizeTunnel(null).configured).toBe(false)
    expect(normalizeTunnel({ configured: 'yes', streams: 'many' }).streams).toBe(0)
    expect(normalizeTunnel({ configured: true, url: 42 }).url).toBe('')
  })
})

describe('describeLink：未配置/断开', () => {
  it('未配置中继：给出「只能在同一局域网访问」+ 文档提示', () => {
    const r = describeLink(direct, tunnel({ configured: false, connected: false, url: '', deviceId: '' }))
    expect(r.tone).toBe('warn')
    expect(r.label).toBe('直连')
    expect(r.summary).toBe(NO_RELAY_HINT)
    expect(r.hint).toContain('外网怎么连')
  })

  it('已配置但隧道断开：说「断开」并带上 relay 的原因，而不是说「未启用」', () => {
    const r = describeLink(direct, tunnel({ connected: false, lastError: '401 票据无效' }))
    expect(r.tone).toBe('warn')
    expect(r.label).toBe('中继断开')
    expect(r.summary).toContain('没建立')
    expect(r.hint).toContain('401 票据无效')
    // 配置是有的，所以不能再劝用户去部署
    expect(r.summary).not.toContain('未配置')
  })

  it('隧道断开的 details 仍然给出设备 id，方便对日志', () => {
    const r = describeLink(direct, tunnel({ connected: false }))
    expect(r.details).toContain('设备 mac-mini-a1b2c3')
  })

  it('接口不可达（旧二进制 503）：只说「未知」，不断言未配置', () => {
    const r = describeLink(direct, null)
    expect(r.tone).toBe('off')
    expect(r.label).toBe('链路未知')
    expect(r.summary).not.toContain('未配置')
    expect(r.summary).toContain('读不到')
  })

  it('页面来自中继但读不到状态：仍然标成经中继', () => {
    const r = describeLink(viaRelay, null)
    expect(r.kind).toBe('relay')
    expect(r.label).toBe('链路未知')
  })
})

describe('describeLink：正常态', () => {
  it('经中继 + 已连接：ok，细节里给设备 id、地址与重连次数', () => {
    const r = describeLink(viaRelay, tunnel({ reconnects: 2, streams: 1 }))
    expect(r.kind).toBe('relay')
    expect(r.tone).toBe('ok')
    expect(r.label).toBe('经中继')
    expect(r.details).toEqual([
      '设备 mac-mini-a1b2c3',
      'https://relay.example.com',
      '在途 1 条流',
      '已重连 2 次',
    ])
    expect(r.hint).toBeUndefined()
  })

  it('直连 + 中继可用：ok，并说明「不经过 relay」', () => {
    const r = describeLink(direct, tunnel())
    expect(r.kind).toBe('direct')
    expect(r.tone).toBe('ok')
    expect(r.label).toBe('直连')
    expect(r.summary).toContain('不经过 relay')
    expect(r.details).toContain('同一局域网内直连，延迟最低')
  })

  it('直连但地址是公网域名（反代）：不硬说「同一局域网」', () => {
    const r = describeLink({ protocol: 'https:', host: 'astra.example.com', pathname: '/' }, tunnel())
    expect(r.details).not.toContain('同一局域网内直连，延迟最低')
  })
})

describe('relayAccessUrl', () => {
  it('拼出手机该访问的地址（尾斜杠归一）', () => {
    expect(relayAccessUrl(tunnel({ url: 'https://relay.example.com/' }))).toBe(
      'https://relay.example.com/t/mac-mini-a1b2c3/',
    )
  })

  it('未配置或缺设备 id 时不编一个地址出来', () => {
    expect(relayAccessUrl(tunnel({ configured: false }))).toBe('')
    expect(relayAccessUrl(tunnel({ deviceId: '' }))).toBe('')
    expect(relayAccessUrl(tunnel({ url: '' }))).toBe('')
  })
})

describe('transportLabel', () => {
  it('rtc = WebRTC 直连 / P2P；其余 = 中继(WS) / relay', () => {
    expect(transportLabel('rtc')).toEqual({ label: 'WebRTC 直连', peer: 'P2P', tone: 'ok' })
    expect(transportLabel('ws').label).toBe('中继(WS)')
    expect(transportLabel('ws').peer).toBe('relay')
    // 协商期画面确实来自 WS，不能写「协商中」让用户以为还没画面
    expect(transportLabel('negotiating').label).toBe('中继(WS)')
    expect(transportLabel('negotiating').tone).toBe('wait')
  })
})
