import { useState } from 'react';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useLocation, useNavigate } from 'react-router-dom';
import { describe, expect, test } from 'vitest';
import {
  ColumnVisibilitySelect,
  StickyTableHead,
  makeRowClick,
  useColumnVisibility,
  type TableColumn,
} from './DataTable';

describe('useColumnVisibility', () => {
  function Probe({ tableId, columns }: { tableId: string; columns: TableColumn[] }) {
    const { visible, toggle } = useColumnVisibility(tableId, columns);
    return (
      <div>
        <span data-testid="visible">{visible.join(',')}</span>
        {columns.map((c) => (
          <button key={c.id} data-testid={`toggle-${c.id}`} onClick={() => toggle(c.id)}>
            t{c.id}
          </button>
        ))}
      </div>
    );
  }

  test('defaults to all columns and prevents hiding the last one', async () => {
    const columns: TableColumn[] = [
      { id: 'a', label: 'A' },
      { id: 'b', label: 'B' },
    ];
    render(<Probe tableId="probe-1" columns={columns} />);
    expect(screen.getByTestId('visible')).toHaveTextContent('a,b');
    await userEvent.click(screen.getByTestId('toggle-a'));
    expect(screen.getByTestId('visible')).toHaveTextContent('b');
    // Last remaining column cannot be dismissed.
    await userEvent.click(screen.getByTestId('toggle-b'));
    expect(screen.getByTestId('visible')).toHaveTextContent('b');
  });

  test('reveals new columns when the set grows', () => {
    const two: TableColumn[] = [
      { id: 'a', label: 'A' },
      { id: 'b', label: 'B' },
    ];
    const { rerender } = render(<Probe tableId="probe-2" columns={two} />);
    expect(screen.getByTestId('visible')).toHaveTextContent('a,b');
    rerender(<Probe tableId="probe-2" columns={[...two, { id: 'c', label: 'C' }]} />);
    expect(screen.getByTestId('visible')).toHaveTextContent('a,b,c');
  });

  test('persists a reduced selection under the table id', async () => {
    const columns: TableColumn[] = [
      { id: 'a', label: 'A' },
      { id: 'b', label: 'B' },
    ];
    render(<Probe tableId="probe-3" columns={columns} />);
    await userEvent.click(screen.getByTestId('toggle-a'));
    expect(JSON.parse(localStorage.getItem('parapet-columns:probe-3') ?? '')).toEqual(['b']);
  });
});

describe('ColumnVisibilitySelect', () => {
  const columns: TableColumn[] = [
    { id: 'a', label: 'Alpha' },
    { id: 'b', label: 'Beta' },
  ];

  function SelectProbe() {
    const [visible, setVisible] = useState(['a', 'b']);
    const toggle = (id: string) =>
      setVisible((current) =>
        current.includes(id) ? current.filter((c) => c !== id) : [...current, id],
      );
    return (
      <ColumnVisibilitySelect
        columns={columns}
        visible={visible}
        onToggle={toggle}
        testId="cols"
      />
    );
  }

  test('opens, lists columns with check state, and toggles', async () => {
    render(<SelectProbe />);
    await userEvent.click(screen.getByTestId('cols-button'));
    expect(screen.getByTestId('cols-panel')).toBeInTheDocument();
    expect(screen.getByTestId('cols-col-a')).toBeChecked();
    await userEvent.click(screen.getByTestId('cols-col-a'));
    expect(screen.getByTestId('cols-col-a')).not.toBeChecked();
  });
});

describe('makeRowClick', () => {
  // Full row-click contract exercised through a real router so makeRowClick's
  // navigate call behaves like the list pages use it.
  function RouteProbe({ to }: { to: string }) {
    const navigate = useNavigate();
    const onRowClick = makeRowClick(navigate, to);
    return (
      <div>
        <span data-testid="location">{useLocation().pathname}</span>
        <table>
          <tbody>
            <tr data-testid="row" onClick={onRowClick} className="cursor-pointer">
              <td>
                <a data-testid="row-link" href="#keep">
                  link text
                </a>
              </td>
              <td>
                <span data-testid="plain">plain cell</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    );
  }

  test('plain-cell clicks navigate, link clicks do not', async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={['/start']}>
        <Routes>
          <Route path="*" element={<RouteProbe to="/detail" />} />
        </Routes>
      </MemoryRouter>,
    );
    await user.click(screen.getByTestId('plain'));
    expect(screen.getByTestId('location')).toHaveTextContent('/detail');
    // The row guard let the link handle ITS click: remount the probe at the
    // start path and click the link — no route change may follow.
    cleanup();
    render(
      <MemoryRouter initialEntries={['/start']}>
        <Routes>
          <Route path="*" element={<RouteProbe to="/detail" />} />
        </Routes>
      </MemoryRouter>,
    );
    await user.click(screen.getByTestId('row-link'));
    expect(screen.getByTestId('location')).toHaveTextContent('/start');
  });
});

describe('StickyTableHead', () => {
  test('renders thead with sticky class', () => {
    render(
      <table>
        <StickyTableHead>
          <tr>
            <th>H</th>
          </tr>
        </StickyTableHead>
      </table>,
    );
    const th = screen.getByText('H');
    expect(th.closest('thead')?.className).toContain('sticky');
  });
});