// Vista Datasets: tabla de datasets/zvols con acciones por fila.
import { useEffect, useState } from 'react';
import { useData } from '../ui/useData';
import { useApp, errorMessage } from '../ui/store';
import { fmtBytes, fmtDateTime } from '../ui/format';
import { Badge, Spinner } from '../components/ui';
import { IconLock, IconUnlock } from '../components/icons';
import { useModal } from '../components/Modal';
import { subscribeEvents } from '../data/events';
import { getProvider } from '../data';
import { isTrash } from '../ui/trash';

export default function Datasets() {
  const { t, isAdmin, caps, refresh, notify } = useApp();
  const { openModal } = useModal();
  const all = useData((p) => p.getDatasets());
  const loading = all.loading;
  // The recycle bin's datasets are shown in their own section, not as data.
  const data = all.data?.filter((d) => !isTrash(d.name));
  const trash = useData((p) => p.getTrash());
  const ops = useData((p) => p.getLongOps());

  // Operaciones largas en vivo (rewrite…): refresca el indicador por fila
  useEffect(() => subscribeEvents((ev) => {
    if (ev.type === 'longop.update') ops.reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }), []);

  const running = new Set((ops.data ?? []).filter((o) => o.status === 'running').map((o) => o.target));

  const [err, setErr] = useState('');
  const dsAct = async (fn: () => Promise<void>) => {
    setErr('');
    try { await fn(); } catch (e) { setErr(errorMessage(e, t)); }
  };
  const restore = (id: number) => dsAct(async () => {
    const { warnings } = await getProvider().restoreTrash(id);
    trash.reload(); all.reload(); refresh();
    if (warnings.length > 0) setErr(t('trash_restored_warn') + ' ' + warnings.join('; '));
    else notify(t('toast_trash_restored'), 'ok');
  });

  // Glifo de árbol estilo mockup: "├─" para hijos, "└─" para el último hijo
  // de cada padre (la lista ya viene ordenada jerárquicamente del backend).
  const treeGlyph = (name: string, idx: number, all: typeof data): string => {
    if (!all) return '';
    const slash = name.lastIndexOf('/');
    if (slash < 0) return '';
    const parent = name.slice(0, slash);
    const isLast = !all.slice(idx + 1).some((x) => x.name.startsWith(parent + '/') && x.name.split('/').length === name.split('/').length);
    return isLast ? '└─' : '├─';
  };

  return (
    <div className="view">
      {loading && !data && <Spinner label={t('loading')} />}
      <div className="card tblwrap">
        <table className="data">
          <thead>
            <tr>
              <th className="ledcol" /><th className="slack">{t('ds_name')}</th><th>{t('ds_type')}</th><th>{t('ds_comp')}</th>
              <th className="num">{t('ds_used')}</th><th className="num">{t('ds_avail')}</th><th className="num">{t('ds_quota')}</th>
              <th className="hide-md">{t('ds_mountpoint')}</th><th />
            </tr>
          </thead>
          <tbody>
            {(data ?? []).map((d, di) => {
              const rewriting = running.has(d.name);
              const canRewrite = isAdmin && !!caps?.rewrite && d.type === 'fs' &&
                !!d.mountpoint && d.mountpoint !== '—' && d.mountpoint !== '-' && d.mountpoint !== 'none' && d.mountpoint !== 'legacy';
              const encrypted = !!d.encryption && d.encryption !== 'off' && d.encryption !== '-';
              const unlocked = d.keystatus === 'available';
              return (
              <tr className="clickable" key={d.name}
                onClick={() => openModal('propsds', { ds: d })}>
                <td className="ledcol">
                  <span className={`led ${encrypted && !unlocked ? 'a' : d.type === 'volume' ? 'c' : 'g'}`} />
                </td>
                <td className="mono" style={{ fontWeight: 600 }}>
                  {treeGlyph(d.name, di, data) && (
                    <span className="tree" aria-hidden="true">{treeGlyph(d.name, di, data)} </span>
                  )}
                  {encrypted && (
                    <span style={{ display: 'inline-flex', verticalAlign: '-3px', marginRight: 6,
                      color: unlocked ? 'var(--ok)' : 'var(--err)' }}
                      title={unlocked ? t('ds_unlocked') : t('ds_locked')}
                      aria-label={unlocked ? t('ds_unlocked') : t('ds_locked')}>
                      {unlocked ? <IconUnlock size={15} /> : <IconLock size={15} />}
                    </span>
                  )}
                  {d.name}
                  {encrypted && (
                    <Badge tone={unlocked ? 'ok' : 'warn'} style={{ marginLeft: 8 }}>{t('ds_encrypted')}</Badge>
                  )}
                  {rewriting && (
                    <Badge tone="info" style={{ marginLeft: 8 }}>{t('ds_rewrite_running')}</Badge>
                  )}
                </td>
                <td style={{ color: 'var(--text2)' }}>{d.type === 'volume' ? t('ds_vol') : t('ds_fs')}</td>
                <td>{d.compression}</td>
                <td className="num">{fmtBytes(d.used_bytes)}</td>
                <td className="num">{fmtBytes(d.avail_bytes)}</td>
                <td className="num">{d.quota_bytes ? fmtBytes(d.quota_bytes) : <span className="dim">—</span>}</td>
                <td className="mono dim hide-md" style={{ fontSize: 12 }}>{d.mountpoint || '—'}</td>
                <td style={{ whiteSpace: 'nowrap' }}>
                  {encrypted && isAdmin && (<>
                    {!unlocked && (
                      <button className="btn sm" title={t('ds_unlock_hint')}
                        onClick={(e) => { e.stopPropagation(); openModal('unlockds', { ds: d }); }}>
                        {t('ds_unlock')}
                      </button>
                    )}{' '}
                    {unlocked && (
                      <button className="btn sm" title={t('ds_lock_hint')}
                        onClick={(e) => { e.stopPropagation(); openModal('lockds', { ds: d }); }}>
                        {t('ds_lock')}
                      </button>
                    )}{' '}
                    <button className="btn sm" title={t('ds_changekey_hint')}
                      onClick={(e) => { e.stopPropagation(); openModal('changekey', { ds: d }); }}>
                      {t('ds_changekey')}
                    </button>{' '}
                  </>)}
                  <button className="btn sm" onClick={(e) => { e.stopPropagation(); openModal('newsnap', { dataset: d.name }); }}>
                    {t('ds_snapshot')}
                  </button>{' '}
                  {canRewrite && !rewriting && (
                    <button className="btn sm" title={t('ds_rewrite_hint')}
                      onClick={(e) => { e.stopPropagation(); openModal('rewrite', { ds: d }); }}>
                      {t('ds_rewrite')}
                    </button>
                  )}{' '}
                  {isAdmin && (
                    <button className="btn sm danger" onClick={(e) => { e.stopPropagation(); openModal('delds', { name: d.name }); }}>
                      {t('delete')}
                    </button>
                  )}
                  {isAdmin && (
                    <button className="btn sm" title={t('ds_mount')}
                      onClick={(e) => { e.stopPropagation(); dsAct(() => getProvider().mountDataset(d.name)); }}>
                      {t('ds_mount')}
                    </button>
                  )}
                  {isAdmin && (
                    <button className="btn sm" title={t('ds_unmount')}
                      onClick={(e) => { e.stopPropagation(); dsAct(() => getProvider().unmountDataset(d.name)); }}>
                      {t('ds_unmount')}
                    </button>
                  )}
                  {isAdmin && (
                    <button className="btn sm" title={t('ds_rename')}
                      onClick={(e) => { e.stopPropagation(); openModal('renameds', { name: d.name }); }}>
                      {t('ds_rename')}
                    </button>
                  )}
                  {isAdmin && (
                    <button className="btn sm" title={t('ds_promote_hint')}
                      onClick={(e) => { e.stopPropagation(); dsAct(() => getProvider().promoteDataset(d.name)); }}>
                      {t('ds_promote')}
                    </button>
                  )}
                </td>
              </tr>
              );
            })}
          </tbody>
        </table>
        {data && data.length === 0 && <div className="empty">{t('empty')}</div>}
      </div>
      <div className="sect">
        <button className="btn primary" onClick={() => openModal('newds', { vol: false })}>{t('ds_new')}</button>
        <button className="btn" style={{ marginLeft: 8 }} onClick={() => openModal('newds', { vol: true })}>{t('ds_newvol')}</button>
      </div>
      {err && <p className="form-err" role="alert">{err}</p>}
      {(trash.data?.items.length ?? 0) > 0 && (
        <div className="sect">
          <h3>{t('trash_title')}</h3>
          <p className="desc">{t('trash_desc').replace('{days}', String(trash.data?.days ?? 7))}</p>
          <div className="card tblwrap">
            <table className="data">
              <thead>
                <tr>
                  <th className="slack">{t('ds_name')}</th><th className="num">{t('ds_used')}</th>
                  <th className="hide-md">{t('trash_deleted_at')}</th><th>{t('trash_purge_at')}</th><th />
                </tr>
              </thead>
              <tbody>
                {trash.data!.items.map((it) => (
                  <tr key={it.id}>
                    <td className="mono" style={{ fontWeight: 600 }}>{it.original}</td>
                    <td className="num">{it.used_bytes == null ? <span className="dim">—</span> : fmtBytes(it.used_bytes)}</td>
                    <td className="hide-md">{fmtDateTime(it.trashed_at)}</td>
                    <td>
                      {fmtDateTime(it.purge_at)}
                      {it.last_error && <div className="form-err" style={{ fontSize: 12 }}>{t('trash_purge_failed')} {it.last_error}</div>}
                    </td>
                    <td style={{ whiteSpace: 'nowrap' }}>
                      {isAdmin && (<>
                        <button className="btn sm" onClick={() => restore(it.id)}>{t('trash_restore')}</button>{' '}
                        <button className="btn sm danger"
                          onClick={() => openModal('purgetrash', { id: it.id, original: it.original, onDone: () => trash.reload() })}>
                          {t('trash_purge')}
                        </button>
                      </>)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}
