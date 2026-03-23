# @vyprai/loka-sdk

TypeScript SDK for LOKA.

## Install

```bash
npm install @vyprai/loka-sdk
```

## Usage

```typescript
import { LokaClient } from '@vyprai/loka-sdk';

const loka = new LokaClient({ baseUrl: 'http://localhost:6840' });

// Pull image and create session
await loka.pullImage('python:3.12-slim');
const session = await loka.createSession({
  image: 'python:3.12-slim',
  mode: 'execute',
});

// Run commands
const result = await loka.runCommand(session.ID, 'python3', ['-c', 'print(42)']);
console.log(result.Results[0].Stdout); // "42\n"

// Checkpoint and restore
const cp = await loka.createCheckpoint(session.ID, 'light', 'initial');
await loka.restoreCheckpoint(session.ID, cp.ID);

// Cleanup
await loka.destroySession(session.ID);
```

## Approval Flow

```typescript
const session = await loka.createSession({
  image: 'ubuntu:22.04',
  mode: 'ask',
});

const ex = await loka.run(session.ID, { command: 'wget', args: ['http://example.com'] });

if (ex.Status === 'pending_approval') {
  // Approve and add to whitelist for future calls
  await loka.approveExecution(session.ID, ex.ID, true);
}
```
