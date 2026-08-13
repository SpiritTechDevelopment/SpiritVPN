package domain

import "testing"

// baseLink — действующая ссылка в самом обычном состоянии: срок не истёк, квота
// не исчерпана, доставка подтверждена, нода пригодна. Каждый тест ниже портит
// ровно одно поле, чтобы было видно, какой именно факт меняет исход.
func baseLink() LinkInput {
	return LinkInput{
		Now:          tNow,
		ExpiresAt:    tFuture,
		DesiredState: DesiredStatePresent,
		ApplyState:   ApplyStateApplied,
		EntryUsable:  true,
	}
}

func TestLinkStatusOf(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*LinkInput)
		want   LinkStatus
	}{
		{
			name:   "конъюнкция условий пригодности выполнена целиком",
			mutate: func(*LinkInput) {},
			want:   LinkStatus{State: LinkStateReady},
		},
		{
			name:   "истёкший срок блокирует customer целиком",
			mutate: func(in *LinkInput) { in.ExpiresAt = tPast },
			want:   LinkStatus{State: LinkStateBlocked, Reason: BlockReasonTimeExpired},
		},
		{
			// Эффективность access требует current_time < expires_at, поэтому
			// момент истечения уже является блокировкой.
			name:   "момент истечения — уже блокировка",
			mutate: func(in *LinkInput) { in.ExpiresAt = tNow },
			want:   LinkStatus{State: LinkStateBlocked, Reason: BlockReasonTimeExpired},
		},
		{
			name:   "исчерпанная квота блокирует ссылку на своей ноде",
			mutate: func(in *LinkInput) { in.QuotaExhausted = true },
			want:   LinkStatus{State: LinkStateBlocked, Reason: BlockReasonTrafficQuotaExhausted},
		},
		{
			// При одновременно применимых причинах наружу уходит TIME_EXPIRED.
			name: "expiry побеждает quota",
			mutate: func(in *LinkInput) {
				in.ExpiresAt = tPast
				in.QuotaExhausted = true
			},
			want: LinkStatus{State: LinkStateBlocked, Reason: BlockReasonTimeExpired},
		},
		{
			name:   "исчерпавшая попытки доставка",
			mutate: func(in *LinkInput) { in.ApplyState = ApplyStateFailed },
			want:   LinkStatus{State: LinkStateFailed},
		},
		{
			// Рассогласованная проекция ломает одну ссылку, а не ответ.
			name:   "непригодная входная нода",
			mutate: func(in *LinkInput) { in.EntryUsable = false },
			want:   LinkStatus{State: LinkStateFailed},
		},
		{
			// Блокировка сильнее: URI не выдаётся в любом случае, а причина
			// блокировки понятна вызывающему, в отличие от FAILED.
			name: "блокировка сильнее непригодной ноды",
			mutate: func(in *LinkInput) {
				in.ExpiresAt = tPast
				in.EntryUsable = false
			},
			want: LinkStatus{State: LinkStateBlocked, Reason: BlockReasonTimeExpired},
		},
		{
			name:   "операция ещё не доставлена",
			mutate: func(in *LinkInput) { in.ApplyState = ApplyStatePending },
			want:   LinkStatus{State: LinkStatePending},
		},
		{
			name:   "доставка ретраится",
			mutate: func(in *LinkInput) { in.ApplyState = ApplyStateRetrying },
			want:   LinkStatus{State: LinkStatePending},
		},
		{
			// Переходное состояние: блокировка уже снята, но EnsureUserPresent
			// ещё не выпущен или не доставлен. Обещать ссылку здесь можно —
			// в отличие от FAILED, она действительно появится.
			name: "desired ABSENT без действующей блокировки",
			mutate: func(in *LinkInput) {
				in.DesiredState = DesiredStateAbsent
				in.ApplyState = ApplyStateApplied
			},
			want: LinkStatus{State: LinkStatePending},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseLink()
			tt.mutate(&in)

			got := LinkStatusOf(in)
			if got != tt.want {
				t.Fatalf("LinkStatusOf(%+v) = %+v, ожидалось %+v", in, got, tt.want)
			}
		})
	}
}

// Причина существует только у BLOCKED. Проверяется отдельно от таблицы:
// требование сформулировано над всеми состояниями сразу, а не над конкретным
// входом.
func TestLinkStatusReasonOnlyWhenBlocked(t *testing.T) {
	inputs := []LinkInput{
		baseLink(),
		func() LinkInput { in := baseLink(); in.ApplyState = ApplyStateFailed; return in }(),
		func() LinkInput { in := baseLink(); in.ApplyState = ApplyStatePending; return in }(),
		func() LinkInput { in := baseLink(); in.EntryUsable = false; return in }(),
	}

	for _, in := range inputs {
		status := LinkStatusOf(in)
		if status.State == LinkStateBlocked {
			t.Fatalf("вход %+v неожиданно заблокирован", in)
		}
		if status.Reason != BlockReasonNone {
			t.Fatalf("состояние %s несёт причину %q, ожидалась пустая", status.State, status.Reason)
		}
	}
}
