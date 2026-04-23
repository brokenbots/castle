import { render, screen } from '@testing-library/react';
import { Provider } from 'react-redux';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { App } from './App';
import { store } from './store';
import { describe, expect, test } from 'vitest';

describe('App login', () => {
  test('shows login form when token is missing', () => {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/']}>
          <Routes>
            <Route path="/" element={<App />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(screen.getByText('Parapet Login')).toBeInTheDocument();
  });
});
