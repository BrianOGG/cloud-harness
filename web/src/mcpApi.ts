import { req } from './api';

export type MCPServer = {
  name: string;
  transport: string;
  auth: string;
  readonly: boolean;
  state: string;
  tools?: string[];
  warnings?: string[];
  error?: string;
};

const endpoint = (name: string) => '/v1/mcp/servers/' + encodeURIComponent(name);
export const mcpApi = {
  list: (signal?: AbortSignal) => req<MCPServer[]>('/v1/mcp/servers', { signal }),
  connect: (name: string) => req<void>(endpoint(name) + '/connect', { method: 'POST' }),
  disconnect: (name: string) => req<void>(endpoint(name) + '/disconnect', { method: 'POST' }),
  authorize: (name: string) => req<{ url: string }>(endpoint(name) + '/authorize', { method: 'POST' }),
};
