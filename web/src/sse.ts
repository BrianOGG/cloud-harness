export interface HarnessEvent {
  seq: number;
  type: string;
  data: string;
}

export class EventStreamDecoder {
  private readonly decoder = new TextDecoder();
  private line = '';
  private ignoreNextLF = false;
  private seq = 0;
  private type = '';
  private data: string[] = [];

  push(chunk: Uint8Array): HarnessEvent[] {
    return this.consume(this.decoder.decode(chunk, { stream: true }));
  }

  finish(): HarnessEvent[] {
    return this.consume(this.decoder.decode());
  }

  private consume(text: string): HarnessEvent[] {
    const events: HarnessEvent[] = [];
    for (const character of text) {
      if (this.ignoreNextLF) {
        this.ignoreNextLF = false;
        if (character === '\n') continue;
      }
      if (character === '\r') {
        this.commitLine(events);
        this.ignoreNextLF = true;
      } else if (character === '\n') {
        this.commitLine(events);
      } else {
        this.line += character;
      }
    }
    return events;
  }

  private commitLine(events: HarnessEvent[]): void {
    if (!this.line) {
      if (this.type) events.push({ seq: this.seq, type: this.type, data: this.data.join('\n') });
      this.seq = 0;
      this.type = '';
      this.data = [];
      return;
    }

    if (this.line.startsWith('id:')) {
      const value = this.line.slice(3).trim();
      if (/^\d+$/.test(value)) this.seq = Number(value);
    } else if (this.line.startsWith('event:')) {
      this.type = this.line.slice(6).trim();
    } else if (this.line.startsWith('data:')) {
      this.data.push(this.line.slice(5).replace(/^ /, ''));
    }
    this.line = '';
  }
}

export function parseEventStream(text: string): HarnessEvent[] {
  const decoder = new EventStreamDecoder();
  const events = decoder.push(new TextEncoder().encode(text));
  events.push(...decoder.finish());
  return events;
}
