import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { type DataSourceSettings } from '@grafana/data';
import { type PromOptions } from '@grafana/prometheus';

import { ConfigEditor } from './ConfigEditor';

function createDefaultConfigOptions(
  jsonData: Record<string, unknown> = {},
  secureJsonFields: Record<string, boolean> = {}
): DataSourceSettings<PromOptions> {
  return {
    jsonData,
    secureJsonFields,
    access: 'proxy',
  } as unknown as DataSourceSettings<PromOptions>;
}

describe('ConfigEditor', () => {
  it('renders without error', () => {
    expect(() =>
      render(<ConfigEditor onOptionsChange={() => {}} options={createDefaultConfigOptions()} />)
    ).not.toThrow();
  });

  it('renders the Connection, Authentication and Advanced sections', () => {
    render(<ConfigEditor onOptionsChange={() => {}} options={createDefaultConfigOptions()} />);
    expect(screen.getByText('Connection')).toBeInTheDocument();
    expect(screen.getByText('Authentication')).toBeInTheDocument();
    expect(screen.getByText('Advanced')).toBeInTheDocument();
    expect(screen.queryByText('Additional settings')).not.toBeInTheDocument();
    expect(screen.getByLabelText(/^Account name \*/)).toBeInTheDocument();
  });

  it('does not render the intro text or debug logging setting', () => {
    render(<ConfigEditor onOptionsChange={() => {}} options={createDefaultConfigOptions({ debug: true })} />);
    expect(screen.queryByText(/Before you can use/)).not.toBeInTheDocument();
    expect(screen.queryByText('Debug logging')).not.toBeInTheDocument();
  });

  it('does not render the generic Prometheus sections', () => {
    render(<ConfigEditor onOptionsChange={() => {}} options={createDefaultConfigOptions()} />);
    expect(screen.queryByText(/Prometheus server URL/)).not.toBeInTheDocument();
    expect(screen.queryByText('Advanced settings')).not.toBeInTheDocument();
    expect(screen.queryByText(/Basic authentication/i)).not.toBeInTheDocument();
  });

  it('stores the account name in jsonData', async () => {
    const onOptionsChange = jest.fn();
    render(<ConfigEditor onOptionsChange={onOptionsChange} options={createDefaultConfigOptions()} />);
    await userEvent.type(screen.getByLabelText(/^Account name \*/), 'k');
    expect(onOptionsChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ jsonData: expect.objectContaining({ account: 'k' }) })
    );
  });

  it('defaults to bearer token authentication', () => {
    render(<ConfigEditor onOptionsChange={() => {}} options={createDefaultConfigOptions()} />);
    expect(screen.getByLabelText(/^Bearer token \*/)).toBeInTheDocument();
    expect(screen.getByText('Recommended. A single LogicMonitor API token.')).toBeInTheDocument();
    expect(screen.queryByLabelText(/^Access ID \*/)).not.toBeInTheDocument();
  });

  it('stores the token in secureJsonData', async () => {
    const onOptionsChange = jest.fn();
    render(<ConfigEditor onOptionsChange={onOptionsChange} options={createDefaultConfigOptions()} />);
    await userEvent.type(screen.getByLabelText(/^Bearer token \*/), 't');
    expect(onOptionsChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ secureJsonData: expect.objectContaining({ token: 't' }) })
    );
  });

  it('switches the auth type to LMv1', async () => {
    const onOptionsChange = jest.fn();
    render(<ConfigEditor onOptionsChange={onOptionsChange} options={createDefaultConfigOptions()} />);
    await userEvent.click(screen.getByText('LMv1 (access ID and key)'));
    expect(onOptionsChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ jsonData: expect.objectContaining({ authType: 'lmv1' }) })
    );
  });

  it('shows access ID and access key for LMv1 and stores the key in secureJsonData', async () => {
    const onOptionsChange = jest.fn();
    render(
      <ConfigEditor onOptionsChange={onOptionsChange} options={createDefaultConfigOptions({ authType: 'lmv1' })} />
    );
    expect(screen.getByLabelText(/^Access ID \*/)).toBeInTheDocument();
    expect(screen.queryByLabelText(/^Bearer token \*/)).not.toBeInTheDocument();
    expect(screen.getByText('A LogicMonitor API token made of an access ID and an access key.')).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText(/^Access key \*/), 'k');
    expect(onOptionsChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ secureJsonData: expect.objectContaining({ accessKey: 'k' }) })
    );
  });

  it('infers LMv1 for legacy settings with only an access ID and key', () => {
    render(
      <ConfigEditor
        onOptionsChange={() => {}}
        options={createDefaultConfigOptions({ accessId: 'id' }, { accessKey: true })}
      />
    );
    expect(screen.getByLabelText(/^Access ID \*/)).toHaveValue('id');
  });

  it('shows a migratable legacy portal as the account name and persists it', () => {
    const onOptionsChange = jest.fn();
    render(
      <ConfigEditor
        onOptionsChange={onOptionsChange}
        options={createDefaultConfigOptions({ portal: 'https://acme.logicmonitor.com/' })}
      />
    );
    expect(screen.getByLabelText(/^Account name \*/)).toHaveValue('acme');
    expect(onOptionsChange).toHaveBeenCalledWith(
      expect.objectContaining({ jsonData: expect.objectContaining({ account: 'acme' }) })
    );
  });

  it('normalizes a valid account name on blur', async () => {
    const onOptionsChange = jest.fn();
    render(
      <ConfigEditor onOptionsChange={onOptionsChange} options={createDefaultConfigOptions({ account: ' Acme ' })} />
    );
    const input = screen.getByLabelText(/^Account name \*/);
    await userEvent.click(input);
    await userEvent.tab();
    expect(onOptionsChange).toHaveBeenCalledWith(
      expect.objectContaining({ jsonData: expect.objectContaining({ account: 'acme' }) })
    );
  });

  it('does not use a non-LogicMonitor legacy portal as the account name', () => {
    render(<ConfigEditor onOptionsChange={() => {}} options={createDefaultConfigOptions({ portal: 'evil.com' })} />);
    expect(screen.getByLabelText(/^Account name \*/)).toHaveValue('');
  });

  describe('account name field', () => {
    it('renders the description, placeholder and suffix', () => {
      render(<ConfigEditor onOptionsChange={() => {}} options={createDefaultConfigOptions()} />);
      expect(
        screen.getByText('The first part of your portal address. For https://acme.logicmonitor.com, enter acme.')
      ).toBeInTheDocument();
      expect(screen.getByLabelText(/^Account name \*/)).toHaveAttribute('placeholder', 'acme');
      expect(screen.getByText('.logicmonitor.com')).toBeInTheDocument();
      expect(screen.queryByText(/^Portal URL:/)).not.toBeInTheDocument();
    });

    it('shows a live portal URL preview for a valid account', () => {
      render(<ConfigEditor onOptionsChange={() => {}} options={createDefaultConfigOptions({ account: ' Acme ' })} />);
      expect(screen.getByText('Portal URL: https://acme.logicmonitor.com')).toBeInTheDocument();
      expect(screen.queryByText(/not the full domain/)).not.toBeInTheDocument();
    });

    it.each([
      'https://acme.logicmonitor.com',
      'acme.logicmonitor.com',
      'evil.com',
      '127.0.0.1',
      'acme:8080',
      'acme/path',
      'a@b',
    ])('hints to enter only the account name when %s is pasted', (value) => {
      render(<ConfigEditor onOptionsChange={() => {}} options={createDefaultConfigOptions({ account: value })} />);
      expect(screen.getByText('Enter only "acme", not the full domain')).toBeInTheDocument();
      expect(screen.queryByText(/^Portal URL:/)).not.toBeInTheDocument();
    });

    it('stores the pasted value as typed', async () => {
      const onOptionsChange = jest.fn();
      render(<ConfigEditor onOptionsChange={onOptionsChange} options={createDefaultConfigOptions()} />);
      await userEvent.click(screen.getByLabelText(/^Account name \*/));
      await userEvent.paste('https://acme.logicmonitor.com');
      expect(onOptionsChange).toHaveBeenLastCalledWith(
        expect.objectContaining({ jsonData: expect.objectContaining({ account: 'https://acme.logicmonitor.com' }) })
      );
    });

    it('rejects invalid characters', () => {
      render(<ConfigEditor onOptionsChange={() => {}} options={createDefaultConfigOptions({ account: '-acme' })} />);
      expect(screen.getByText(/Use only lowercase letters, numbers and hyphens/)).toBeInTheDocument();
      expect(screen.queryByText(/^Portal URL:/)).not.toBeInTheDocument();
    });
  });

  it('stores the API timeout in jsonData as a number', async () => {
    const onOptionsChange = jest.fn();
    render(<ConfigEditor onOptionsChange={onOptionsChange} options={createDefaultConfigOptions({ lmTimeout: 1 })} />);
    await userEvent.click(screen.getByRole('button', { name: 'Expand section Advanced' }));
    const input = screen.getByLabelText(/^Timeout/);
    expect(input).toHaveValue(1);
    expect(input).toHaveAttribute('placeholder', '60');
    expect(screen.getByText(/Time allowed for each LogicMonitor API call.* Default: 60 seconds\./)).toBeInTheDocument();
    await userEvent.type(input, '2');
    expect(onOptionsChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ jsonData: expect.objectContaining({ lmTimeout: 12 }) })
    );
  });

  it('flags an out-of-range LogicMonitor API timeout', async () => {
    render(<ConfigEditor onOptionsChange={() => {}} options={createDefaultConfigOptions({ lmTimeout: 601 })} />);
    await userEvent.click(screen.getByRole('button', { name: 'Expand section Advanced' }));
    expect(screen.getByText('Enter a whole number between 1 and 600.')).toBeInTheDocument();
  });

  describe('Advanced section', () => {
    it('is collapsed by default and lists the limits with their defaults', async () => {
      render(<ConfigEditor onOptionsChange={() => {}} options={createDefaultConfigOptions()} />);
      expect(screen.getByText('Advanced')).toBeInTheDocument();
      expect(screen.queryByLabelText(/^Timeout/)).not.toBeInTheDocument();
      expect(screen.queryByLabelText(/^Parallel chunks/)).not.toBeInTheDocument();
      await userEvent.click(screen.getByRole('button', { name: 'Expand section Advanced' }));
      expect(screen.getByText('Requests')).toBeInTheDocument();
      expect(screen.getByText('Retries and rate limiting')).toBeInTheDocument();
      expect(screen.getByText('Metadata cache')).toBeInTheDocument();
      expect(screen.getByLabelText(/^Timeout/)).toHaveAttribute('placeholder', '60');
      expect(screen.getByLabelText(/^Parallel chunks/)).toHaveAttribute('placeholder', '10');
      expect(screen.getByLabelText(/^Concurrent requests/)).toHaveAttribute('placeholder', '16');
      expect(screen.getByLabelText(/^Attempts per request/)).toHaveAttribute('placeholder', '3');
      expect(screen.getByLabelText(/^Rate-limit wait/)).toHaveAttribute('placeholder', '2');
      expect(screen.getByLabelText(/^Cache TTL/)).toHaveAttribute('placeholder', '60');
      expect(screen.getByLabelText(/^Cache size/)).toHaveAttribute('placeholder', '1000');
      expect(screen.getByText(/Default: 10\.$/)).toBeInTheDocument();
    });

    it('is collapsed by default even when limits are set and stores limits in jsonData as numbers', async () => {
      const onOptionsChange = jest.fn();
      render(
        <ConfigEditor
          onOptionsChange={onOptionsChange}
          options={createDefaultConfigOptions({ queryRangeWorkers: 4 })}
        />
      );
      expect(screen.queryByLabelText(/^Parallel chunks/)).not.toBeInTheDocument();
      await userEvent.click(screen.getByRole('button', { name: 'Expand section Advanced' }));
      const input = screen.getByLabelText(/^Parallel chunks/);
      expect(input).toHaveValue(4);
      await userEvent.type(screen.getByLabelText(/^Rate-limit wait/), '5');
      expect(onOptionsChange).toHaveBeenLastCalledWith(
        expect.objectContaining({ jsonData: expect.objectContaining({ maxRateLimitWaitSeconds: 5 }) })
      );
      await userEvent.clear(input);
      expect(onOptionsChange).toHaveBeenLastCalledWith(
        expect.objectContaining({ jsonData: expect.objectContaining({ queryRangeWorkers: undefined }) })
      );
    });

    it('flags out-of-range limits', async () => {
      render(
        <ConfigEditor
          onOptionsChange={() => {}}
          options={createDefaultConfigOptions({ maxConcurrentRequests: 101, cacheTTLSeconds: 1.5 })}
        />
      );
      await userEvent.click(screen.getByRole('button', { name: 'Expand section Advanced' }));
      expect(screen.getByText('Enter a whole number between 1 and 100.')).toBeInTheDocument();
      expect(screen.getByText('Enter a whole number between 1 and 3600.')).toBeInTheDocument();
    });
  });
});
