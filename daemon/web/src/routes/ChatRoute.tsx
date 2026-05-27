// SPDX-License-Identifier: Apache-2.0

import { useParams } from "react-router-dom";
import { ChatLayout } from "../components/ChatLayout";
import { type WhoAmI } from "../auth/types";

export function ChatRoute({ who }: { who: WhoAmI }) {
  const params = useParams<{ conversationId?: string }>();
  return (
    <ChatLayout
      who={who}
      initialConversation={params.conversationId ?? null}
    />
  );
}
