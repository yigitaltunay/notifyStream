CREATE INDEX idx_notifications_list_filter ON notifications (
    status,
    channel,
    created_at DESC,
    id DESC
);
