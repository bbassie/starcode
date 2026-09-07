-- The first builds with the context meter guessed the window from the
-- model id and wrote the guess with every reading, and the guess was
-- wrong for models whose id does not say "[1m]" (Fable 5.1 has 1M). From
-- here on a session only reports a window the agent stated, so what is
-- stored now may be a guess: drop it, and the model catalog answers until
-- the next turn reports one.
UPDATE threads SET context_window = 0;
