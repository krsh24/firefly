---
title: WSAck
---
{% include-markdown "./_includes/wsack_description.md" %}

### Example

```json
{
    "type": "ack",
    "id": "f78bf82b-1292-4c86-8a08-e53d855f1a64",
    "subscription": {
        "namespace": "ns1",
        "name": "app1_subscription"
    }
}
```

### Field Descriptions

| Field Name | Description | Type |
|------------|-------------|------|
| `type` | WSActionBase.type | `FFEnum`:<br/>`"start"`<br/>`"ack"`<br/>`"protocol_error"`<br/>`"event_batch"` |
| `id` | WSAck.id | [`UUID`](simpletypes.md#uuid) |
| `subscription` | WSAck.subscription | [`SubscriptionRef`](#subscriptionref) |

## SubscriptionRef

| Field Name | Description | Type |
|------------|-------------|------|
| `id` | The UUID of the subscription | [`UUID`](simpletypes.md#uuid) |
| `namespace` | The namespace of the subscription. A subscription will only receive events generated in the namespace of the subscription | `string` |
| `name` | The name of the subscription. The application specifies this name when it connects, in order to attach to the subscription and receive events that arrived while it was disconnected. If multiple apps connect to the same subscription, events are workload balanced across the connected application instances | `string` |


