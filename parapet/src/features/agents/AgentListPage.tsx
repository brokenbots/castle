import { useMemo } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import {
  useListAgentsQuery,
  AGENT_STATUS_TEXT_COLORS,
  ColumnVisibilitySelect,
  StickyTableHead,
  makeRowClick,
  useColumnVisibility,
  type TableColumn,
  type Agent,
} from '@castle/run-viewer';
import { classifyError } from '@castle/run-viewer';
import { PageHeader } from '@castle/run-viewer';
import { PageState } from '@castle/run-viewer';

export function AgentListPage() {
  const navigate = useNavigate();
  const { data, isLoading, error, refetch } = useListAgentsQuery();
  const errorKind = error ? classifyError(error) : undefined;

  const AGENT_COLUMNS: TableColumn[] = useMemo(
    () => [
      { id: 'name', label: 'Name' },
      { id: 'hostname', label: 'Hostname' },
      { id: 'status', label: 'Status' },
      { id: 'last-seen', label: 'Last seen' },
    ],
    [],
  );
  const { visible: visibleColumns, toggle: toggleColumn } = useColumnVisibility('agents', AGENT_COLUMNS);
  const show = (id: string) => visibleColumns.includes(id);

  const agentTo = (a: Agent): string => `/agents/${encodeURIComponent(a.criteriaId)}`;

  return (
    <div>
      <PageHeader
        title="Agents"
        actions={
          <ColumnVisibilitySelect
            columns={AGENT_COLUMNS}
            visible={visibleColumns}
            onToggle={toggleColumn}
            testId="agent-columns"
          />
        }
      />
      {isLoading ? (
        <PageState loading title="Loading agents…" testId="agent-list-loading" />
      ) : error ? (
        errorKind === 'unauthenticated' ? (
          <PageState error kind="unauthenticated" />
        ) : (
          <PageState
            error
            kind={errorKind}
            title="Failed to load agents."
            detail="Castle is unreachable or failed to answer. Try again."
            onRetry={() => void refetch()}
            testId="agent-list-error"
          />
        )
      ) : (data ?? []).length === 0 ? (
        <PageState
          empty
          title="No agents yet."
          detail="Agents appear here once they register with Castle."
          testId="agent-list-empty"
        />
      ) : (
        <table className="w-full text-sm" data-testid="agent-list-table">
          <StickyTableHead>
            <tr className="border-b border-line">
              {show('name') && <th className="py-2 pr-4 font-medium">Name</th>}
              {show('hostname') && <th className="py-2 pr-4 font-medium">Hostname</th>}
              {show('status') && <th className="py-2 pr-4 font-medium">Status</th>}
              {show('last-seen') && <th className="py-2 text-right font-medium">Last seen</th>}
            </tr>
          </StickyTableHead>
          <tbody>
            {(data ?? []).map((a) => (
              <tr
                key={a.criteriaId}
                data-testid="agent-list-row"
                className="cursor-pointer border-b border-line transition-colors hover:bg-surface-raised/50"
                onClick={makeRowClick(navigate, agentTo(a))}
              >
                {show('name') && (
                  <td className="py-2 pr-4">
                    {/* Entries link into the agent detail view (/agents/:criteriaId). */}
                    <Link
                      to={agentTo(a)}
                      className="text-ink hover:text-accent-strong hover:underline"
                    >
                      {a.name}
                    </Link>
                  </td>
                )}
                {show('hostname') && (
                  <td className="py-2 pr-4 text-ink-muted">{a.labels.hostname ?? ''}</td>
                )}
                {show('status') && (
                  <td className={`py-2 pr-4 ${AGENT_STATUS_TEXT_COLORS[a.status] ?? 'text-ink-muted'}`}>
                    {a.status}
                  </td>
                )}
                {show('last-seen') && (
                  <td className="py-2 text-right text-ink-muted">
                    {a.lastSeenAt ? new Date(a.lastSeenAt).toLocaleString() : ''}
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}