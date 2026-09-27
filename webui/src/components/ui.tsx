import React from 'react';

export function Panel({ title, subtitle, children }: React.PropsWithChildren<{ title: string; subtitle?: string }>) {
  return <section className="panel"><div className="panel-heading"><p className="eyebrow">HAOSBOT CONTROL PLANE</p><h1>{title}</h1>{subtitle && <p>{subtitle}</p>}</div>{children}</section>;
}

export function Stat({ title, value, detail }: { title: string; value: React.ReactNode; detail?: string }) {
  return <div className="stat"><span>{title}</span><strong>{value ?? '—'}</strong>{detail && <small>{detail}</small>}</div>;
}
