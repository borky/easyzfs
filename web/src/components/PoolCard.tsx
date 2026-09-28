// Tarjeta de pool compartida entre Panel y Pools (réplica del mockup)
import { getProvider } from '../data';
import type { Disk, Pool, Topo } from '../data/types';
import { ApiError } from '../data/types';
import { errorMessage, useApp } from '../ui/store';
import { t } from '../ui/i18n';
import { fmtBytes, fmtBytesPair, fmtPct, fmtRatio, timeAgo } from '../ui/format';
import { statusLabel } from '../ui/labels';
import { Badge, InfoBubble, Switch } from './ui';
import { Sparkline } from './Sparkline';
import { useModal } from './Modal';
import { useEffect, useState } from 'react';

// topoTipKey — clave i18n que explica la topología (ayuda contextual U4).
const topoTipKey = (topo: Topo) => {
  switch (topo) {
    case 'mirror': return 'topo_mirror';
    case 'raidz1': return 'topo_raidz1';
    case 'raidz2': return 'topo_raidz2';
    default: return 'topo_stripe';
  }
};

// topoBase — topología base de la cadena descriptiva de pool.topo
// ("raidz2 (4×1,86 TB NVMe)" → "raidz2").
const topoBase = (s: string): Topo | null => {
  const m = s.match(/^(mirror|raidz1|raidz2|stripe)/);
  return m ? (m[1] as Topo) : null;
};

// TopoHelp — burbuja que explica la topología del pool según la base de la
// cadena; si no se reconoce, no muestra nada.
function TopoHelp({ topo }: { topo: string }) {
  const base = topoBase(topo);
  if (!base) return null;
  return <InfoBubble title={topo}>{t(topoTipKey(base))}</InfoBubble>;
}

// shortDev — nombre corto de vdev: ruta base si la hay; UUID acortado si no.
const shortDev = (v: { dev: string; path?: string }) =>
  v.path ? v.path.replace('/dev/', '') : (v.dev.length > 13 ? v.dev.slice(0, 8) + '…' : v.dev);

// fmtEta — "~45 min" / "~13 h 23 m" según magnitud.
const fmtEta = (sec: number): string => {
  const min = Math.max(1, Math.round(sec / 60));
  if (min < 90) return `~${min} min`;
  return `~${Math.floor(min / 60)} h ${min % 60} m`;
};

export function PoolCard({ pool, onChanged }: { pool: Pool; onChanged: () => void }) {
  const { t, isAdmin, caps, notify } = useApp();
  const { openModal } = useModal();
  const [err, setErr] = useState('');
  const [disks, setDisks] = useState<Disk[]>([]);
  const [trimOverride, setTrimOverride] = useState<boolean | null>(null);
  const [hist, setHist] = useState<{ days: number; points: { ts: number; value: number }[] }[]>([]);
  const pct = pool.total_bytes > 0 ? (pool.used_bytes / pool.total_bytes) * 100 : 0;
  const cap = fmtBytesPair(pool.used_bytes, pool.total_bytes);
  const running = pool.scrub.state === 'running';
  const resilvering = running && pool.scrub.kind === 'resilver';
  const expanding = running && pool.scrub.kind === 'expand';
  const ok = pool.status === 'ONLINE';

  useEffect(() => {
    let alive = true;
    getProvider().getDisks().then((d) => { if (alive) setDisks(d); }).catch(() => {});
    const src = `pool.${pool.name}.used_pct`;
    getProvider().getSeries(src, 7, 120)
      .then((r) => { if (alive) setHist((cur) => [...cur.filter((h) => h.days !== 7), { days: 7, points: r.points }]); })
      .catch(() => {});
    getProvider().getSeries(src, 30, 160)
      .then((r) => { if (alive) setHist((cur) => [...cur.filter((h) => h.days !== 30), { days: 30, points: r.points }]); })
      .catch(() => {});
    return () => { alive = false; };
  }, [pool]);

  // Cuando el dato real alcanza el valor pedido, suelta el override optimista.
  useEffect(() => {
    setTrimOverride((cur) => (cur !== null && cur === pool.autotrim ? null : cur));
  }, [pool.autotrim]);

  const scrub = async (action: 'start' | 'pause' | 'stop') => {
    setErr('');
    try {
      await getProvider().scrubAction(pool.name, action);
      onChanged();
      notify(t('toast_action_done'), 'ok');
    } catch (e) { const m = errorMessage(e, t); setErr(m); notify(m, 'err'); }
  };

  // risk — the server's warning (409 risk_ack_required) for taking a disk
  // out of a pool that is degraded or resilvering; shown with a way on.
  const [risk, setRisk] = useState<{ dev: string; action: 'offline' | 'online'; msg: string } | null>(null);
  const vdevAct = async (dev: string, action: 'offline' | 'online', ack = false) => {
    setErr(''); setRisk(null);
    try {
      await getProvider().vdevAction(pool.name, dev, action, undefined, ack);
      onChanged();
      notify(t('toast_action_done'), 'ok');
    } catch (e) {
      if (e instanceof ApiError && e.code === 'risk_ack_required') { setRisk({ dev, action, msg: e.message }); return; }
      const m = errorMessage(e, t); setErr(m); notify(m, 'err');
    }
  };

  const toggleAutotrim = async () => {
    setErr('');
    // Estado optimista: el Switch es controlado y la caché del backend puede
    // tardar unos segundos en reflejar el cambio; sin override local el
    // interruptor "vuelve solo" y parece que no hace nada. Se limpia cuando
    // la prop pool.autotrim alcanza el valor pedido.
    const next = !(trimOverride ?? pool.autotrim);
    setTrimOverride(next);
    try {
      await getProvider().setAutotrim(pool.name, next);
      onChanged();
    } catch (e) { setTrimOverride(null); const m = errorMessage(e, t); setErr(m); notify(m, 'err'); }
  };

  const faulted = pool.vdevs.find((v) => v.status !== 'ONLINE' && !v.replacing);
  const free = disks.filter((d) => (d.pool === '—' || d.pool === '') && !d.in_use);
  const isMirror = pool.topo.startsWith('mirror');
  // RAID-Z expansion: solo con capability, vdev raidz detectado y discos libres.
  const canExpand = isAdmin && !!caps?.raidz_expansion &&
    (pool.raidz_vdevs ?? []).length > 0 && free.length > 0 && !expanding;

  const ledCls = ok ? 'g' : pool.status === 'DEGRADED' ? 'a' : 'r';
  // The host's own storage (analysis §21), read from the live system: the
  // pool the running OS is on keeps only maintenance; one holding Proxmox
  // storage cannot be exported or destroyed. The server refuses the same.
  const hostSys = pool.host === 'system';
  const lockedTitle = pool.host_reason ?? '';

  return (
    <div className="card">
      <div className="pool-head">
        <span className={`led ${ledCls}`} />
        <div className="grow">
          <div className="pool-name">
            {pool.name} <Badge tone={ok ? 'ok' : 'warn'}>{statusLabel(pool.status, t)}</Badge>
            {/* warn, not info: a checkpoint blocks zpool replace and hot spares */}
            {pool.checkpoint && <Badge tone="warn">{t('ck_badge')}</Badge>}
            {pool.host && (
              <span title={pool.host_reason}>
                <Badge tone="info">{t(hostSys ? 'host_badge_system' : 'host_badge_storage')}</Badge>
              </span>
            )}
          </div>
          <div className="pool-raid">{pool.topo} <TopoHelp topo={pool.topo} /></div>
        </div>
        <div className="pool-cap">
          <div className="big">
            {cap.used}
            <span className="dim">{' '}{t('pool_of')} {cap.total}</span>
          </div>
          <div className="pct">{fmtPct(pct)} {t('pool_used')}</div>
        </div>
      </div>

      <div className="pool-bar">
        <div className={`segbar ${pct >= 90 ? 'crit' : pct >= 80 ? 'warn' : ''}`}
          role="progressbar" aria-valuenow={Math.round(pct)} aria-valuemin={0} aria-valuemax={100}>
          <i style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} />
        </div>
        {hist.length > 0 && (
          <div className="sparks" aria-label={t('pool_history_series')}>
            {hist.sort((a, b) => a.days - b.days).map((h) => (
              <span key={h.days} className="spark" title={t('pool_history_days', { days: h.days })}>
                <Sparkline points={h.points} width={72} height={26}
                  ariaLabel={t('pool_history_days', { days: h.days })} />
                <em>{h.days}d</em>
              </span>
            ))}
          </div>
        )}
      </div>

      <div className="poolmeta">
        <span>{t('pool_comp')} <b>{fmtRatio(pool.comp_ratio)}</b></span>
        <span>{t('pool_frag')} <b>{fmtPct(pool.frag_pct)}</b></span>
        <span>
          {t('pool_last_scrub')}{' '}
          <b>{running ? t('pool_in_progress') : timeAgo(pool.scrub.ts, t)}</b>
          {' '}· <b>{pool.scrub.errors} {t('pool_errors')}</b>
        </span>
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 7 }}>
          <Switch checked={trimOverride ?? pool.autotrim} onChange={() => { void toggleAutotrim(); }}
            disabled={!isAdmin} ariaLabel={t('pool_autotrim')} />
          <b>{t('pool_autotrim')}</b>
          <InfoBubble title={t('pool_autotrim')}>{t('pool_autotrim_hint')}</InfoBubble>
        </span>
      </div>

      {running && (<>
        <div style={{ padding: '0 16px 4px', fontSize: 12.5, color: 'var(--info)', fontWeight: 600 }}>
          {expanding ? t('pool_expand_running') : resilvering ? t('pool_resilvering') : pool.scrub.kind === 'trim' ? t('pool_trim_running') : t('pool_scrub_running')}
          {' '}· {Math.round(pool.scrub.pct)}%
          {pool.scrub.eta_sec > 0 && <> · {fmtEta(pool.scrub.eta_sec)}</>}
          {(pool.scrub.bytes_done ?? 0) > 0 && (pool.scrub.bytes_total ?? 0) > 0 && (
            <> · {t('pool_scan_bytes', { done: fmtBytes(pool.scrub.bytes_done ?? 0), total: fmtBytes(pool.scrub.bytes_total ?? 0) })}</>
          )}
        </div>
        <div className="scrubbar" role="progressbar" aria-valuenow={Math.round(pool.scrub.pct)} aria-valuemin={0} aria-valuemax={100}>
          <i style={{ width: `${pool.scrub.pct}%` }} />
        </div>
      </>)}

      {isAdmin && !running && faulted && free.length > 0 && (
        <div className="rebuildbar">
          <span style={{ flex: 1, minWidth: 220 }}>
            {t('pool_rebuild', {
              dev: free[0].dev,
              size: fmtBytes(free[0].size_bytes),
              old: shortDev(faulted),
            })}
          </span>
          <button className="btn sm warn" title={hostSys ? lockedTitle : t('pool_rebuild_hint')} disabled={hostSys}
            onClick={() => openModal('replace', { pool: pool.name, oldDev: faulted.dev, newDev: free[0].dev })}>
            {t('pool_rebuild_btn')}
          </button>
        </div>
      )}

      <div className="vdevs">
        {[...pool.vdevs]
          .sort((a, b) => Number(a.replacing && a.status !== 'ONLINE') - Number(b.replacing && b.status !== 'ONLINE'))
          .map((v) => {
          // Hijo saliente de un replacing-N: no es un error, es el disco viejo
          // que desaparece solo al terminar la reconstrucción.
          if (v.replacing && v.status !== 'ONLINE') {
            return (
              <div className="vdev out" key={v.dev}>
                <span className="led o" />
                <span className="badge info" style={{ padding: '2px 7px' }}>{t('vdev_outgoing')}</span>
                <span className="dname" title={v.dev}>{shortDev(v)}</span>
                <span className="drole">{t('vdev_outgoing_hint')}</span>
              </div>
            );
          }
          return (
          <div className={`vdev${v.status !== 'ONLINE' ? ' faulted' : ''}`} key={v.dev} style={v.replacing ? { flexWrap: 'wrap' } : undefined}>
            <span className={`led ${v.status === 'ONLINE' ? 'g' : v.status === 'OFFLINE' ? 'o' : 'r'}`} />
            <span className={`badge ${v.status === 'ONLINE' ? 'ok' : 'err'}`} style={{ padding: '2px 7px' }}>{statusLabel(v.status, t)}</span>
            <span className="dname" title={v.dev}>{shortDev(v)}</span>
            {v.replacing && (
              <span className="badge info" style={{ padding: '1px 7px' }} title={t('vdev_new_hint')}>{t('vdev_new')}</span>
            )}
            <span className="drole">{v.role !== '—' ? v.role : ''}</span>
            <span className="temp" style={{ marginLeft: 'auto' }}>{v.temp_c}°C</span>
            {v.replacing ? (
              <span className="vdevjoin" title={t('vdev_joining_hint')}>
                {t('vdev_joining')} · {Math.round(pool.scrub.pct)}%
              </span>
            ) : (<>
            <button className="btn sm" disabled={!isAdmin || hostSys}
              title={!isAdmin ? t('no_permission') : hostSys ? lockedTitle : t('pool_replace_disk', { dev: shortDev(v) })}
              onClick={() => openModal('replace', { pool: pool.name, oldDev: v.dev })}>{t('pool_replace')}</button>
            {v.status === 'ONLINE' && (
              <button className="btn sm" disabled={!isAdmin || hostSys} title={!isAdmin ? t('no_permission') : hostSys ? lockedTitle : t('vdev_offline_hint')}
                onClick={() => vdevAct(v.dev, 'offline')}>{t('vdev_offline')}</button>
            )}
            {v.status === 'OFFLINE' && (
              <button className="btn sm" disabled={!isAdmin} title={!isAdmin ? t('no_permission') : undefined}
                onClick={() => vdevAct(v.dev, 'online')}>{t('vdev_online')}</button>
            )}
            {isMirror && (
              <button className="btn sm danger" disabled={!isAdmin || hostSys} title={!isAdmin ? t('no_permission') : hostSys ? lockedTitle : t('vdev_detach_hint')}
                onClick={() => openModal('detach', { pool: pool.name, dev: v.dev, path: v.path })}>{t('vdev_detach')}</button>
            )}
            </>)}
            {v.replacing && (
              <div className={`vdevbar ${pool.scrub.pct <= 30 ? 'low' : pool.scrub.pct <= 90 ? 'mid' : 'high'}`}>
                <i style={{ width: `${Math.max(1, pool.scrub.pct)}%` }} />
              </div>
            )}
          </div>
          );
        })}
      </div>

      {hostSys && <p className="desc" style={{ padding: '0 16px' }}>{t('host_system_note')}</p>}
      {err && <p className="form-err" style={{ padding: '0 16px' }} role="alert">{err}</p>}
      {risk && (
        <div className="form-err" style={{ padding: '0 16px' }} role="alert">
          <p>{risk.msg}</p>
          <button type="button" className="btn sm danger" onClick={() => vdevAct(risk.dev, risk.action, true)}>{t('risk_continue')}</button>{' '}
          <button type="button" className="btn sm" onClick={() => setRisk(null)}>{t('cancel')}</button>
        </div>
      )}

      <div className="pool-actions">
        {!resilvering && !expanding && (
          <button className="btn sm" title={running ? t('pool_scrub_pause_hint') : t('pool_scrub_hint')}
            onClick={() => scrub(running ? 'pause' : 'start')}>
            {running ? t('pool_scrub_pause') : t('pool_scrub_now')}
          </button>
        )}
        {running && !resilvering && !expanding && <button className="btn sm" onClick={() => scrub('stop')}>{t('pool_scrub_stop')}</button>}
        <button className="btn sm" title={t('pool_history_hint')}
          onClick={() => openModal('history', { pool: pool.name })}>{t('pool_history')}</button>
        <button className="btn sm" disabled={!isAdmin || hostSys} title={!isAdmin ? t('no_permission') : hostSys ? lockedTitle : t('ck_title')}
          onClick={() => openModal('checkpoint', { pool: pool.name, active: pool.checkpoint })}>{t('pool_checkpoint')}</button>
        <button className="btn sm" disabled={!isAdmin || hostSys} title={!isAdmin ? t('no_permission') : hostSys ? lockedTitle : t('pool_add_vdev_hint')}
          onClick={() => openModal('addvdev', { pool: pool.name })}>{t('pool_add_vdev')}</button>
        {canExpand && (
          <button className="btn sm" title={hostSys ? lockedTitle : t('xpd_hint')} disabled={hostSys}
            onClick={() => openModal('expand', { pool })}>{t('xpd_btn')}</button>
        )}
        <button className="btn sm" disabled={!isAdmin || !!pool.host} title={!isAdmin ? t('no_permission') : pool.host ? lockedTitle : t('pool_export_hint')}
          onClick={() => openModal('export', { pool: pool.name })}>
          {t('pool_export')}
        </button>
        <button className="btn sm" disabled={!isAdmin} title={t('pool_clear_hint')}
          onClick={() => getProvider().clearPool(pool.name).then(() => { onChanged(); notify(t('toast_action_done'), 'ok'); }).catch((e: Error) => { setErr(e.message); notify(e.message, 'err'); })}>
          {t('pool_clear')}
        </button>
      </div>
    </div>
  );
}
