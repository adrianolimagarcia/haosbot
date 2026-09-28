import type { State } from '../../api';
import type { View } from '../../types';
import { Panel } from '../ui';
import { Channels } from './Channels';
import { Apps } from './Apps';
import { Skills } from './Skills';
import { Automations } from './Automations';
import { Models } from './Models';
import { Memory } from './Memory';
import { Files } from './Files';
import { Runtime } from './Runtime';
import { Settings } from './Settings';

export function Workspace({ view, state, refresh, report }: { view: View; state: State | null; refresh: () => Promise<void>; report: (error: string) => void }) {
  if (!state) return <Panel title="Carregando…"/>;
  switch (view) {
    case 'channels': return <Channels state={state} refresh={refresh} report={report}/>;
    case 'apps': return <Apps state={state} refresh={refresh} report={report}/>;
    case 'skills': return <Skills state={state} refresh={refresh} report={report}/>;
    case 'automations': return <Automations report={report}/>;
    case 'models': return <Models state={state} refresh={refresh} report={report}/>;
    case 'memory': return <Memory state={state} report={report}/>;
    case 'files': return <Files/>;
    case 'runtime': return <Runtime state={state}/>;
    default: return <Settings state={state}/>;
  }
}
